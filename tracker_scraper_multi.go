package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/tracker"
	"github.com/anacrolix/torrent/types/infohash"
)

// Announces a torrent to a tracker at regular intervals, when peers are
// required.

type trackerStatus struct {
	t            *Torrent
	lastAnnounce *trackerAnnounceResult
	interval     time.Duration
}

type multiTrackerScraper struct {
	u               url.URL
	cl              *Client
	t               map[infohash.T]*trackerStatus
	mu              sync.RWMutex
	ips             []net.IP
	lookupTrackerIp func(*url.URL) ([]net.IP, error)
}

func (me *multiTrackerScraper) URL() *url.URL {
	return &me.u
}

func (ts *multiTrackerScraper) statusLine(t *trackerStatus) string {
	var w bytes.Buffer
	fmt.Fprintf(&w, "next ann: %v, last ann: %v",
		func() string {
			ts.mu.RLock()
			defer ts.mu.RUnlock()
			na := time.Until(t.lastAnnounce.Completed.Add(t.lastAnnounce.Interval))
			if na > 0 {
				na /= time.Second
				na *= time.Second
				return na.String()
			} else {
				return "anytime"
			}
		}(),
		func() string {
			ts.mu.RLock()
			defer ts.mu.RUnlock()
			if t.lastAnnounce.Err != nil {
				return t.lastAnnounce.Err.Error()
			}
			if t.lastAnnounce.Completed.IsZero() {
				return "never"
			}
			return fmt.Sprintf("%d peers", t.lastAnnounce.NumPeers)
		}(),
	)
	return w.String()
}

func (me *multiTrackerScraper) getIp() (ip net.IP, err error) {
	if len(me.ips) == 0 {
		if me.lookupTrackerIp != nil {
			me.ips, err = me.lookupTrackerIp(&me.u)
		} else {
			// Do a regular dns lookup
			me.ips, err = net.LookupIP(me.u.Hostname())
		}
		if err != nil {
			return
		}
		if len(me.ips) == 0 {
			err = errors.New("no ips")
			return
		}
	}

	me.cl.rLock()
	defer me.cl.rUnlock()
	if me.cl.closed.IsSet() {
		err = errors.New("client is closed")
		return
	}
	for _, ip = range me.ips {
		if me.cl.ipIsBlocked(ip) {
			continue
		}
		switch me.u.Scheme {
		case "udp4":
			if ip.To4() == nil {
				continue
			}
		case "udp6":
			if ip.To4() != nil {
				continue
			}
		}
		return
	}

	return nil, errors.New("no acceptable ips")
}

func (me *multiTrackerScraper) trackerUrl(ip net.IP) string {
	u := me.u
	if u.Port() != "" {
		u.Host = net.JoinHostPort(ip.String(), u.Port())
	}
	return u.String()
}

// Return how long to wait before trying again. For most errors, we return 5
// minutes, a relatively quick turn around for DNS changes.
func (me *multiTrackerScraper) announce(ctx context.Context, t *Torrent, event tracker.AnnounceEvent) (ret trackerAnnounceResult) {
	defer func() {
		ret.Completed = time.Now()
	}()
	ret.Interval = time.Minute

	// Limit concurrent use of the same tracker URL by the Client.
	ref := me.cl.activeAnnounceLimiter.GetRef(me.u.String())
	defer ref.Drop()
	select {
	case <-ctx.Done():
		ret.Err = ctx.Err()
		return
	case ref.C() <- struct{}{}:
	}
	defer func() {
		select {
		case <-ref.C():
		default:
			panic("should return immediately")
		}
	}()

	ip, err := me.getIp()
	if err != nil {
		ret.Err = fmt.Errorf("error getting ip: %s", err)
		return
	}
	req := t.announceRequest(event, true, true)
	// The default timeout works well as backpressure on concurrent access to the tracker. Since
	// we're passing our own Context now, we will include that timeout ourselves to maintain similar
	// behavior to previously, albeit with this context now being cancelled when the Torrent is
	// closed.
	ctx, cancel := context.WithTimeout(ctx, tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	t.logger.WithDefaultLevel(log.Debug).Printf("announcing to %q: %#v", me.u.String(), req)
	res, err := tracker.Announce{
		Context:             ctx,
		HttpProxy:           me.cl.config.HTTPProxy,
		HttpRequestDirector: me.cl.config.HttpRequestDirector,
		DialContext:         me.cl.config.TrackerDialContext,
		ListenPacket:        me.cl.config.TrackerListenPacket,
		UserAgent:           me.cl.config.HTTPUserAgent,
		TrackerUrl:          me.trackerUrl(ip),
		Request:             req,
		HostHeader:          me.u.Host,
		ServerName:          me.u.Hostname(),
		UdpNetwork:          me.u.Scheme,
		ClientIp4:           krpc.NodeAddr{IP: me.cl.config.PublicIp4},
		ClientIp6:           krpc.NodeAddr{IP: me.cl.config.PublicIp6},
		Logger:              t.logger,
	}.Do()
	t.logger.WithDefaultLevel(log.Debug).Printf("announce to %q returned %#v: %v", me.u.String(), res, err)
	if err != nil {
		ret.Err = fmt.Errorf("announcing: %w", err)
		return
	}
	t.AddPeers(peerInfos(nil).AppendFromTracker(res.Peers))
	ret.NumPeers = len(res.Peers)
	ret.Interval = time.Duration(res.Interval) * time.Second
	return
}

// Returns whether we can shorten the interval, and sets notify to a channel that receives when we
// might change our mind, or leaves it if we won't.
func (me *multiTrackerScraper) canIgnoreInterval(t *Torrent, notify *<-chan struct{}) bool {
	gotInfo := t.GotInfo()
	select {
	case <-gotInfo:
		// Private trackers really don't like us announcing more than they specify. They're also
		// tracking us very carefully, so it's best to comply.
		private := t.info.Private
		return private == nil || !*private
	default:
		*notify = gotInfo
		return false
	}
}

func (me *multiTrackerScraper) AddTorrent(ctx context.Context, t *Torrent) {
	me.mu.Lock()

	if _, ok := me.t[t.infoHash]; ok {
		me.mu.Unlock()
		return
	}

	ts := &trackerStatus{t: t}
	me.t[t.infoHash] = ts
	me.mu.Unlock()

	ar := me.announce(ctx, t, tracker.Started)

	me.mu.Lock()
	ts.lastAnnounce = &ar
	me.mu.Unlock()
}

func (me *multiTrackerScraper) Run(ctx context.Context) {
	defer func() {
		for _, ts := range me.t {
			me.announceStopped(ts.t)
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

RECALCULATE:
	for {
		cases := []reflect.SelectCase{{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		}}

		const complete = 0
		const recalculate = 1
		const reselect = 2

		actions := map[int]func() int{
			0: func() int { return complete },
		}

		for th, ts := range me.t {
			th, ts := th, ts

			// Make sure we don't announce for at least a minute since the last one.

			ts.t.mu.Lock()
			wantPeers := ts.t.wantPeersEvent.C()
			ts.t.mu.Unlock()

			// If we want peers, reduce the interval to the minimum if it's appropriate.

			// A channel that receives when we should reconsider our interval. Starts as nil since that
			// never receives.
			var reconsider <-chan struct{}
			select {
			case <-wantPeers:
				if ts.interval > time.Minute && me.canIgnoreInterval(ts.t, &reconsider) {
					ts.interval = time.Minute
				}
			default:
				reconsider = wantPeers
			}

			if reconsider != nil {
				actions[len(cases)] = func() int {
					ts.interval = ts.lastAnnounce.Interval
					if ts.interval < time.Minute {
						ts.interval = time.Minute
					}
					return recalculate
				}

				cases = append(cases,
					reflect.SelectCase{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(reconsider),
					})

			}

			actions[len(cases)] = func() int {
				me.mu.Lock()
				delete(me.t, th)
				me.mu.Unlock()
				if len(me.t) == 0 {
					return complete
				}
				return recalculate
			}

			cases = append(cases,
				reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(ts.t.closed.Done()),
				})

			actions[len(cases)] = func() int {
				ar := me.announce(ctx, ts.t, tracker.None)
				me.mu.Lock()
				ts.lastAnnounce = &ar
				me.mu.Unlock()
				return reselect
			}

			cases = append(cases,
				reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(time.After(time.Until(ts.lastAnnounce.Completed.Add(ts.interval)))),
				})
		}

	RESELECT:
		for {
			selected, _, _ := reflect.Select(cases)

			if action, ok := actions[selected]; ok {
				switch action() {
				case complete:
					return
				case recalculate:
					continue RECALCULATE
				default:
					continue RESELECT
				}
			}
		}
	}
}

func (me *multiTrackerScraper) announceStopped(t *Torrent) {
	ctx, cancel := context.WithTimeout(context.Background(), tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	me.announce(ctx, t, tracker.Stopped)
}
