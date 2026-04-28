package publisher

import (
	"context"
	"net"
	"strings"
	"sync/atomic"

	srt "github.com/datarhei/gosrt"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/sessions"
)

// playSession is the publisher-side adapter around sessions.Tracker. It hides
// the nil-tracker case (feature disabled) and exposes a per-session bytes
// counter the protocol-specific write loops increment after every successful
// outbound write. Close runs once and credits the final byte total to the
// session record before emitting EventSessionClosed.
type playSession struct {
	closer  sessions.Closer
	bytes   atomic.Int64
	disable bool
}

// noopPlaySession is what we return when the tracker is unconfigured. add /
// close are cheap atomic ops; the path is hot enough (every TS chunk for
// SRT, every NAL for RTSP) that we don't want a nil check in the caller.
func noopPlaySession() *playSession { return &playSession{disable: true} }

func (p *playSession) add(n int64) {
	if p.disable || n <= 0 {
		return
	}
	p.bytes.Add(n)
}

func (p *playSession) close() {
	if p.disable || p.closer == nil {
		return
	}
	p.closer.Close(domain.SessionCloseClient, p.bytes.Load())
}

// openSRTSession opens a tracker session for one SRT play subscriber. Streamid
// can carry a `?token=…` for operator-issued auth tokens — extracted via
// sessions.TokenFromQuery so the session is correctly tagged named_by="token".
func openSRTSession(ctx context.Context, t sessions.Tracker, code domain.StreamCode, conn srt.Conn) *playSession {
	if t == nil {
		return noopPlaySession()
	}
	streamID := conn.StreamId()
	token := ""
	if i := strings.IndexByte(streamID, '?'); i >= 0 {
		token = sessions.TokenFromQuery(streamID[i+1:])
	}
	_, c := t.OpenConn(ctx, sessions.ConnHit{
		StreamCode: code,
		Protocol:   domain.SessionProtoSRT,
		RemoteAddr: conn.RemoteAddr().String(),
		Token:      token,
	})
	return &playSession{closer: c}
}

// openRTMPSession opens a tracker session for an external RTMP play client.
// The remote addr / flashVer come from the RTMP server's per-conn metadata
// captured at OnPlay handshake time.
func openRTMPSession(ctx context.Context, t sessions.Tracker, code domain.StreamCode, remoteAddr, flashVer string) *playSession {
	if t == nil {
		return noopPlaySession()
	}
	_, c := t.OpenConn(ctx, sessions.ConnHit{
		StreamCode: code,
		Protocol:   domain.SessionProtoRTMP,
		RemoteAddr: remoteAddr,
		UserAgent:  flashVer,
	})
	return &playSession{closer: c}
}

// openRTSPSession opens a tracker session for one RTSP client (one ServerSession
// from gortsplib). UserAgent is the RTSP "User-Agent" request header.
func openRTSPSession(ctx context.Context, t sessions.Tracker, code domain.StreamCode, remote net.Addr, userAgent string) *playSession {
	if t == nil {
		return noopPlaySession()
	}
	_, c := t.OpenConn(ctx, sessions.ConnHit{
		StreamCode: code,
		Protocol:   domain.SessionProtoRTSP,
		RemoteAddr: remote.String(),
		UserAgent:  userAgent,
	})
	return &playSession{closer: c}
}
