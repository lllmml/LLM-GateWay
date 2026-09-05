//go:build integration

// ADR-018 D8 ambiguous-outcome verification against the REAL pinned Redis
// through a test-only drop proxy. go-redis connects through the proxy to the
// real Redis. A unique per-run script forces the EVALSHA -> NOSCRIPT -> EVAL
// path; the proxy relays everything until it sees the NOSCRIPT error reply,
// then forwards the fallback EVAL (which the real Redis executes) and drops
// the connection before any reply is relayed - a genuine ambiguous outcome.
// The server-side counter proves exactly how many times the mutating script
// ran, which is the observable consequence of any forbidden retransmission.
package distributed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// connState carries the per-connection drop decision.
type connState struct {
	armed atomic.Bool // set once the proxy relayed the EVALSHA NOSCRIPT error
}

// dropProxy forwards client <-> real Redis traffic. Per connection, once the
// EVALSHA NOSCRIPT reply has been relayed, the next forwarded client command
// (the EVAL fallback) is executed by the real Redis but its reply is never
// relayed: the connection is closed, making the EVAL outcome ambiguous.
type dropProxy struct {
	upstreamAddr string
	listener     net.Listener
	accepted     atomic.Int64
	lastError    atomic.Value // debug: last '-'-prefixed reply text
}

func newDropProxy(t *testing.T, upstreamAddr string) *dropProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxy := &dropProxy{upstreamAddr: upstreamAddr, listener: listener}
	go proxy.acceptLoop()
	t.Cleanup(func() { _ = listener.Close() })
	return proxy
}

func (p *dropProxy) acceptLoop() {
	p.lastError.Store("")
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		go p.handle(conn)
	}
}

func (p *dropProxy) handle(conn net.Conn) {
	defer conn.Close()
	upstream, err := net.Dial("tcp", p.upstreamAddr)
	if err != nil {
		return
	}
	defer upstream.Close()
	state := &connState{}

	// Relay goroutine: forwards upstream replies. When it relays the EVALSHA
	// NOSCRIPT error, it arms the drop so the EVAL fallback reply is discarded.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		buffer := make([]byte, 32*1024)
		for {
			n, err := upstream.Read(buffer)
			if n > 0 {
				chunk := buffer[:n]
				noscript := bytes.HasPrefix(chunk, []byte("-NOSCRIPT"))
				switch {
				case noscript:
					// Relay the NOSCRIPT error (go-redis needs it to fall back
					// to EVAL), then arm the drop for that EVAL's reply.
					p.lastError.Store(string(chunk))
					_, _ = conn.Write(chunk)
					state.armed.Store(true)
				case state.armed.Load():
					// The ambiguous EVAL already executed; drop its reply.
				default:
					_, _ = conn.Write(chunk)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	buffer := make([]byte, 32*1024)
	var armedSince time.Time
	for {
		if state.armed.Load() && !armedSince.IsZero() && time.Since(armedSince) > 50*time.Millisecond {
			// The EVAL fallback and any trailing request bytes have been
			// forwarded; the real Redis has executed it. Drop now, before any
			// reply is relayed -> ambiguous outcome.
			return
		}
		if state.armed.Load() && armedSince.IsZero() {
			// Drain the rest of the request (an EVAL body can span segments)
			// before dropping.
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			armedSince = time.Now()
		}
		n, err := conn.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if _, writeErr := upstream.Write(chunk); writeErr != nil {
				return
			}
			if state.armed.Load() {
				armedSince = time.Now() // more request bytes arrived; keep draining
			}
			continue
		}
		if err != nil {
			return
		}
	}
}

// TestZeroRetryNeverRetransmitsAmbiguousMutatingEval runs a real mutating EVAL
// (Script.Run: EVALSHA -> NOSCRIPT -> EVAL) through the drop proxy with the
// production MaxRetries=-1 posture. The script must execute on the real Redis
// exactly once (counter == 1) and the client must observe exactly one
// connection: the ambiguous outcome is never retransmitted.
func TestZeroRetryNeverRetransmitsAmbiguousMutatingEval(t *testing.T) {
	realClient := testRedisClient(t)
	counterKey := "ambiguous:" + t.Name() + ":counter"
	ctx := context.Background()
	t.Cleanup(func() { _ = realClient.Del(ctx, counterKey).Err() })

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	body := "-- unique " + hex.EncodeToString(nonce) + "\nreturn redis.call('INCR', KEYS[1])"

	proxy := newDropProxy(t, realClient.Options().Addr)
	proxyClient := redis.NewClient(&redis.Options{
		Addr:         proxy.listener.Addr().String(),
		MaxRetries:   -1, // the production posture (0 would mean 3 retries)
		DialTimeout:  time.Second,
		ReadTimeout:  800 * time.Millisecond,
		WriteTimeout: time.Second,
	})
	defer proxyClient.Close()

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := redis.NewScript(body).Run(runCtx, proxyClient, []string{counterKey}).Err()
	if err == nil {
		t.Fatal("ambiguous mutating EVAL unexpectedly succeeded")
	}

	time.Sleep(200 * time.Millisecond) // give any forbidden retransmission time
	executions, getErr := realClient.Get(ctx, counterKey).Int64()
	if getErr != nil {
		t.Fatalf("read execution counter: %v (last proxy error=%q)", getErr, proxy.lastError.Load().(string))
	}
	if executions != 1 {
		t.Fatalf("mutating EVAL executed %d times, want exactly 1 (ambiguous outcome was retransmitted)", executions)
	}
	if got := proxy.accepted.Load(); got != 1 {
		t.Fatalf("client opened %d connections, want exactly 1", got)
	}
}

// TestDefaultRetryRetransmitsAmbiguousMutatingEval documents why -1 is
// mandatory: with the go-redis default retry posture the same ambiguous
// mutating EVAL is retransmitted and executes more than once on the real
// Redis (double charge), which is exactly the outcome ADR-018 D8 forbids.
func TestDefaultRetryRetransmitsAmbiguousMutatingEval(t *testing.T) {
	realClient := testRedisClient(t)
	counterKey := "ambiguous:" + t.Name() + ":counter"
	ctx := context.Background()
	t.Cleanup(func() { _ = realClient.Del(ctx, counterKey).Err() })

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	body := "-- unique " + hex.EncodeToString(nonce) + "\nreturn redis.call('INCR', KEYS[1])"

	proxy := newDropProxy(t, realClient.Options().Addr)
	proxyClient := redis.NewClient(&redis.Options{
		Addr:         proxy.listener.Addr().String(),
		DialTimeout:  time.Second,
		ReadTimeout:  400 * time.Millisecond,
		WriteTimeout: time.Second,
		// MaxRetries unset -> normalized to the go-redis default of 3.
	})
	defer proxyClient.Close()

	runCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_ = redis.NewScript(body).Run(runCtx, proxyClient, []string{counterKey}).Err() // error expected

	time.Sleep(300 * time.Millisecond)
	executions, getErr := realClient.Get(ctx, counterKey).Int64()
	if getErr != nil {
		t.Fatalf("read execution counter: %v (last proxy error=%q)", getErr, proxy.lastError.Load().(string))
	}
	if executions < 2 {
		t.Fatalf("default-retry client executed the mutating EVAL %d times, want >= 2 (retransmission demonstrated)", executions)
	}
	if got := proxy.accepted.Load(); got <= 1 {
		t.Fatalf("default-retry client opened %d connections, want > 1", got)
	}
}
