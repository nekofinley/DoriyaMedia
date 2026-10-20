package netproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// check.go: "does this proxy actually work" as a question a settings page can
// ask, answered in the terms the user is worried about (region, not bytes).
//
// The check dials the proxy, fetches bilibili's own zone endpoint through it and
// reports where bilibili thinks the request came from. That is the exact fact
// the region check turns on: a 番剧 answers -10403 when the request arrives from
// outside the licensed region, and this endpoint states the country the request
// was seen in without needing an episode to test against.

// biliZoneURL reports the requester's country as bilibili sees it.
const biliZoneURL = "https://api.bilibili.com/x/web-interface/zone"

// Check is the outcome of probing a proxy.
type Check struct {
	// OK is true when the proxy answered and bilibili's API was reached.
	OK bool `json:"ok"`
	// Proxy is the proxy in summary form (no secrets).
	Proxy string `json:"proxy"`
	// Country / Region is where bilibili saw the request from.
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	// IP is the address bilibili saw.
	IP string `json:"ip,omitempty"`
	// Mainland is true when that address is inside mainland China, which is
	// where the licensed 番剧 play without restriction.
	Mainland bool `json:"mainland"`
	// HMT is true when that address is in Hong Kong, Macau or Taiwan. Those are
	// licensed separately from the mainland: they unlock the 港澳台 titles and
	// the mainland-only ones answer -10403, so neither answer is simply "wrong".
	HMT bool `json:"hmt"`
	// Latency is how long the round trip took.
	LatencyMS int64 `json:"latency_ms"`
	// Message is one line fit for a settings page.
	Message string `json:"message"`
	// Err is set when the check failed.
	Err string `json:"error,omitempty"`
}

// CheckProxy dials through the spec and asks bilibili where it is. An empty
// spec checks the server's own route, which is what "no proxy" means here.
func CheckProxy(spec Spec, timeout time.Duration) Check {
	out := Check{Proxy: spec.Summary()}
	if spec.Raw == "" {
		out.Proxy = "直连(不使用代理)"
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	var client *http.Client
	if spec.Raw == "" {
		client = &http.Client{Timeout: timeout}
	} else {
		c, err := NewClient(spec, timeout)
		if err != nil {
			out.Err = err.Error()
			out.Message = "代理配置无法使用: " + err.Error()
			return out
		}
		client = c
	}

	began := time.Now()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, biliZoneURL, nil)
	if err != nil {
		out.Err = err.Error()
		out.Message = out.Err
		return out
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) DoriyaMedia")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	resp, err := client.Do(req)
	out.LatencyMS = time.Since(began).Milliseconds()
	if err != nil {
		out.Err = tidyNetErr(err)
		out.Message = "连不上 bilibili(经 " + out.Proxy + "): " + out.Err
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		out.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		out.Message = "bilibili 接口返回 " + out.Err
		return out
	}
	country, region, ip, ok := parseZone(body)
	if !ok {
		out.Err = "bilibili 的地区接口返回了无法解析的内容"
		out.Message = out.Err
		return out
	}
	out.OK = true
	out.Country, out.Region, out.IP = country, region, ip
	out.Mainland = isMainland(country, region)
	out.HMT = isHMT(country, region)
	switch {
	case out.Mainland:
		out.Message = fmt.Sprintf("代理可用,bilibili 看到的地区是%s%s(%s)——可以看正版番剧", country, region, ip)
	case out.HMT:
		out.Message = fmt.Sprintf("代理可用,bilibili 看到的地区是%s%s(%s)——能看港澳台版权的番剧;大陆独家的仍会报地区限制(-10403),可在源上开启「只上架当前线路能播的番剧」", country, region, ip)
	default:
		out.Message = fmt.Sprintf("代理可用,但 bilibili 看到的地区是%s%s(%s)——大部分正版番剧会报地区限制(-10403),请换一个落地在中国大陆或港澳台的节点", country, region, ip)
	}
	return out
}

// parseZone reads the three fields the endpoint states, accepting the plain
// numbers the API may send for zone ids.
func parseZone(body []byte) (country, region, ip string, ok bool) {
	text := string(body)
	country = jsonString(text, "country")
	province := jsonString(text, "province")
	city := jsonString(text, "city")
	ip = jsonString(text, "addr")
	if country == "" && ip == "" {
		return "", "", "", false
	}
	region = province
	if city != "" && city != province {
		region += city
	}
	return country, region, ip, true
}

// jsonString pulls one string field out of a small JSON document without
// pulling in a decoder: the reply's shape is fixed and shallow, and this keeps
// the check readable.
func jsonString(doc, key string) string {
	needle := `"` + key + `"`
	i := strings.Index(doc, needle)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(needle):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// isMainland reports whether a bilibili zone answer is inside mainland China.
// The country name comes back in Chinese ("中国") or as a code ("CN"), and the
// province is what distinguishes Hong Kong / Macau / Taiwan, which are licensed
// separately and are NOT mainland for 番剧 purposes.
func isMainland(country, region string) bool {
	c := strings.TrimSpace(country)
	if c != "中国" && !strings.EqualFold(c, "CN") && !strings.EqualFold(c, "China") {
		return false
	}
	return !isHMT(country, region)
}

// isHMT reports whether a zone answer is Hong Kong, Macau or Taiwan. The
// endpoint may put the special region in the province field ("中国 香港") or in
// the country field itself ("中国台湾"), depending on the route, so both are
// checked.
func isHMT(country, region string) bool {
	area := strings.TrimSpace(country) + " " + strings.TrimSpace(region)
	for _, name := range []string{
		"香港", "澳门", "澳門", "台湾", "臺灣",
		"Hong Kong", "Macau", "Macao", "Taiwan",
	} {
		if strings.Contains(area, name) {
			return true
		}
	}
	return false
}

// tidyNetErr turns the url.Error wrapper's inner cause into something a
// settings page can show, and says which hop failed.
func tidyNetErr(err error) string {
	text := err.Error()
	if ue, ok := err.(*url.Error); ok && ue.Err != nil {
		text = ue.Err.Error()
	}
	switch {
	case strings.Contains(text, "connection refused"):
		return "连接被拒绝(代理没启动,或端口不对)"
	case strings.Contains(text, "i/o timeout"), strings.Contains(text, "deadline exceeded"):
		return "连接超时(代理不通,或节点连不上 bilibili)"
	case strings.Contains(text, "certificate"), strings.Contains(text, "x509"):
		return "TLS 证书校验失败(可能被中间人劫持,或系统时间不对): " + text
	}
	return text
}

// DialCheck reports whether the proxy endpoint itself accepts a TCP connection.
// It is the fast answer to "is the tunnel client running at all", and it is
// what the settings page shows before the slower zone check.
func DialCheck(spec Spec, timeout time.Duration) error {
	if spec.Raw == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	conn, err := net.DialTimeout("tcp", spec.Host, timeout)
	if err != nil {
		return fmt.Errorf("连不上代理 %s(请确认代理软件已启动): %w", spec.Host, err)
	}
	_ = conn.Close()
	return nil
}

// unused keeps the tls import honest if the config below is trimmed.
var _ = tls.VersionTLS12
