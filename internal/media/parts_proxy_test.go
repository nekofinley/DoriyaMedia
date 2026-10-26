package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type invalidProxySpecDriver struct {
	fakeDriver
	target      string
	directCalls atomic.Int32
	probeCalls  atomic.Int32
}

func (d *invalidProxySpecDriver) ProxySpec() string {
	return "ftp://proxy.example.invalid:21"
}

func (d *invalidProxySpecDriver) DirectURL(context.Context, string) (string, map[string]string, error) {
	d.directCalls.Add(1)
	return d.target, nil, nil
}

func (d *invalidProxySpecDriver) ProbeURL(context.Context, string) (string, map[string]string, error) {
	d.probeCalls.Add(1)
	return d.target, nil, nil
}

func TestSourceProxyForRejectsInvalidSpec(t *testing.T) {
	drv := &invalidProxySpecDriver{target: "http://127.0.0.1:1/should-not-dial"}
	proxy, err := sourceProxyFor(drv)
	if err == nil {
		t.Fatalf("invalid proxy spec was accepted as %q", proxy)
	}
	if proxy != "" {
		t.Fatalf("invalid proxy spec returned bridge %q", proxy)
	}
}

func TestInvalidSourceProxySpecDoesNotFallBackToDirectDial(t *testing.T) {
	var directHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		_, _ = w.Write([]byte("direct media bytes"))
	}))
	t.Cleanup(upstream.Close)

	drv := &invalidProxySpecDriver{target: upstream.URL + "/media.mkv"}
	var probeExecCalls atomic.Int32
	e := newTestEngine(t, func(ctx context.Context, _ *Prober, target string, _ map[string]string, _ string) (*Probe, error) {
		probeExecCalls.Add(1)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Transport: &http.Transport{Proxy: nil}}
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	})

	inputs, err := e.SegmentInputs(context.Background(), testFile(), drv, "")
	if err == nil {
		t.Fatalf("SegmentInputs accepted invalid proxy and returned %+v", inputs)
	}
	if len(inputs) != 0 {
		t.Fatalf("SegmentInputs returned direct inputs after proxy failure: %+v", inputs)
	}
	if got := drv.probeCalls.Load(); got != 0 {
		t.Fatalf("SegmentInputs resolved the direct target %d time(s)", got)
	}

	if _, err := e.EnsureProbe(context.Background(), testFile(), drv); err == nil {
		t.Fatal("EnsureProbe accepted invalid proxy")
	}
	if got := probeExecCalls.Load(); got != 0 {
		t.Fatalf("probe executed %d time(s) without a working source proxy", got)
	}
	if got := directHits.Load(); got != 0 {
		t.Fatalf("invalid proxy fallback made %d direct HTTP round trip(s)", got)
	}
}
