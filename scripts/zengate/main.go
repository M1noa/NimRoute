// zengate probes opencode zen's free-tier gate: the two fingerprints a request
// has to carry (the <env> block + `read` tool in the body, and the client's TLS
// hello) and the exits that still have budget.
//
// go run ./scripts/zengate -proxy http://1.2.3.4:8080
// go run ./scripts/zengate -sweep              # dial the pool list, report who serves
// go run ./scripts/zengate -proxy <url> -variant no-env
//
// the gate is pre-auth and pre-quota, so every refusal is the same
// `429 FreeUsageLimitError` whatever went wrong, and a spent exit looks exactly
// like a shape regression. the way to tell them apart: -sweep over the whole
// list. some exit answering 200 (or -probe answering 400) means the shape is
// still good and the pool is simply spent; every exit refusing -probe means the
// request shape drifted. keep the hello below in sync with zentls.go — it is
// copied, not shared, and a mismatched hello reads as a dead pool.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// endpoint is zen's chat path. /zen/v1 answers 403 FreeTierError for everyone.
const endpoint = "https://opencode.ai/inference/v1/chat/completions"

// endpointURL is what post() dials; -endpoint retargets it at a local sniffer so
// this tool's own ClientHello can be captured and diffed against the client's.
var endpointURL = endpoint

// poolList is the third-party exit list (same urls zenproxy.go fetches).
const (
	poolList     = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&response_max=30000"
	poolFallback = "https://raw.githubusercontent.com/M1noa/proxypool/refs/heads/output/proxies.json"
)

// --- the TLS hello (copy of zentls.go's spec) ---

var cipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,
	tls.TLS_AES_256_GCM_SHA384,
	tls.TLS_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
}

func helloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       cipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				// captured off the released 1.19.4 client 2026-10-27: the list grew two
				// entries (ed25519, ecdsa_secp521r1_sha512) and the gate reads it.
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519MLKEM768},
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
		},
	}
}

// --- tunnels ---

// forceV6 makes direct dials go out over ipv6: a second address family is a
// second free-tier bucket, so it doubles as a clean bench for shape tests.
var forceV6 bool

func dialTunnel(addr, proxyURL string, d time.Duration) (net.Conn, error) {
	if proxyURL == "" {
		netw := "tcp"
		if forceV6 {
			netw = "tcp6"
		}
		return net.DialTimeout(netw, addr, d)
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	switch pu.Scheme {
	case "socks4", "socks4a":
		return dialSocks4(addr, pu.Host, d)
	case "socks5", "socks5h":
		return dialSocks5(addr, pu, d)
	}
	return dialConnect(addr, proxyURL, d)
}

func dialConnect(addr, proxyURL string, d time.Duration) (net.Conn, error) {
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", pu.Host, d)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(d))
	var b bytes.Buffer
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if pu.User != nil {
		pw, _ := pu.User.Password()
		cred := pu.User.Username() + ":" + pw
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(cred)))
	}
	b.WriteString("\r\n")
	if _, err := c.Write(b.Bytes()); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		c.Close()
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	code := 0
	if len(parts) >= 2 {
		code, _ = strconv.Atoi(parts[1])
	}
	if code != 200 {
		c.Close()
		return nil, fmt.Errorf("connect refused %q", strings.TrimSpace(line))
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			c.Close()
			return nil, err
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func dialSocks4(addr, proxyAddr string, d time.Duration) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", proxyAddr, d)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(d))
	req := []byte{0x04, 0x01, byte(port >> 8), byte(port), 0, 0, 0, 0x01, 0}
	req = append(req, host...)
	req = append(req, 0)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[1] != 0x5a {
		c.Close()
		return nil, fmt.Errorf("socks4 status %d", resp[1])
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func dialSocks5(addr string, pu *url.URL, d time.Duration) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", pu.Host, d)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(d))
	hello := []byte{0x05, 0x01, 0x00}
	var user, pass string
	if pu.User != nil {
		user = pu.User.Username()
		pass, _ = pu.User.Password()
		hello = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := c.Write(hello); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[1] == 0x02 {
		var b bytes.Buffer
		b.WriteByte(0x01)
		b.WriteByte(byte(len(user)))
		b.WriteString(user)
		b.WriteByte(byte(len(pass)))
		b.WriteString(pass)
		if _, err := c.Write(b.Bytes()); err != nil {
			c.Close()
			return nil, err
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(c, ar); err != nil || ar[1] != 0 {
			c.Close()
			return nil, fmt.Errorf("socks5 auth refused")
		}
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		c.Close()
		return nil, err
	}
	if head[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks5 reply %d", head[1])
	}
	skip := 4
	switch head[3] {
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			c.Close()
			return nil, err
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	}
	if _, err := io.ReadFull(c, make([]byte, skip+2)); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func httpClient(proxyURL string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := dialTunnel(addr, proxyURL, timeout)
			if err != nil {
				return nil, err
			}
			raw.SetDeadline(time.Now().Add(timeout))
			c := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloCustom)
			if err := c.ApplyPreset(helloSpec()); err != nil {
				raw.Close()
				return nil, err
			}
			if err := c.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			raw.SetDeadline(time.Time{})
			return c, nil
		},
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// --- request shape ---

// opencode ids: ts*0x1000+seq, complemented when descending, plus 14 base62.
var (
	idMu   sync.Mutex
	idTS   int64
	idSeq  int64
	idMask int64 = 0xFFFFFFFFFFFF
)

func opencodeID(descending bool) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ts := time.Now().UnixMilli()
	idMu.Lock()
	if ts != idTS {
		idTS = ts
		idSeq = 0
	}
	idSeq++
	masked := (ts*0x1000 + idSeq) & idMask
	if descending {
		masked = (^masked) & idMask
	}
	idMu.Unlock()
	rb := make([]byte, 14)
	rand.Read(rb)
	for i := range rb {
		rb[i] = chars[int(rb[i])%62]
	}
	return fmt.Sprintf("%012x", masked) + string(rb)
}

// readTool is the v2 client's read declaration. the gate only matches the name.
var readTool = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}

// clientEnv is the real client's block: a heading and five fields.
const clientEnv = "Here is some useful information about the environment you are running in:\n" +
	"<env>\n  Working directory: /Users/you/project\n  Workspace root folder: /Users/you/project\n" +
	"  Is directory a git repo: yes\n  Platform: darwin\n  Today's date: Mon Oct 26 2026\n</env>"

// minimalEnv is what nimroute injects: the one line the gate reads.
const minimalEnv = "<env>\n  Working directory:\n</env>"

// variants of the client's own block, to find which line the gate reads: the
// heading, the field set, or a Working directory with a value.
const (
	envNoHeading   = "<env>\n  Working directory: /Users/you/project\n  Workspace root folder: /Users/you/project\n  Is directory a git repo: yes\n  Platform: darwin\n  Today's date: Mon Oct 26 2026\n</env>"
	envHeadingWD   = "Here is some useful information about the environment you are running in:\n<env>\n  Working directory: /Users/you/project\n</env>"
	envRealWD      = "<env>\n  Working directory: /Users/you/project\n</env>"
	envHeadingOnly = "Here is some useful information about the environment you are running in:\n<env>\n</env>"
)

var variants = map[string]func(model string) map[string]any{
	"full": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+clientEnv, true)
	},
	"minimal": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+minimalEnv, true)
	},
	"env-noheading": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+envNoHeading, true)
	},
	"env-heading-wd": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+envHeadingWD, true)
	},
	"env-real-wd": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+envRealWD, true)
	},
	"env-heading-only": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+envHeadingOnly, true)
	},
	"full-notools": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+clientEnv, false)
	},
	"no-env": func(model string) map[string]any {
		return chatBody(model, "You are opencode.", true)
	},
	"no-tools": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n"+minimalEnv, false)
	},
	"empty-env": func(model string) map[string]any {
		return chatBody(model, "You are opencode.\n\n<env>\n</env>", true)
	},
	"env-in-user": func(model string) map[string]any {
		b := chatBody(model, "You are opencode.", true)
		b["messages"] = []any{
			map[string]any{"role": "system", "content": "You are opencode."},
			map[string]any{"role": "user", "content": minimalEnv + "\n\nhi"},
		}
		return b
	},
	// what a body that already brings its own tools gets. the question these
	// answer: does the gate want the *name* `read`, or any toolset at all?
	"client-tools": func(model string) map[string]any {
		b := chatBody(model, "You are opencode.\n\n"+minimalEnv, false)
		b["tools"] = []any{tool("get_weather", "Get the weather for a city")}
		return b
	},
	"read-capital": func(model string) map[string]any {
		b := chatBody(model, "You are opencode.\n\n"+minimalEnv, false)
		b["tools"] = []any{tool("Read", "Read a file")}
		return b
	},
	"read-renamed": func(model string) map[string]any {
		b := chatBody(model, "You are opencode.\n\n"+minimalEnv, false)
		b["tools"] = []any{tool("fileread", "Read a file")}
		return b
	},
	"read-placeholder": func(model string) map[string]any {
		b := chatBody(model, "You are opencode.\n\n"+minimalEnv, false)
		d := readTool["function"].(map[string]any)
		plain := map[string]any{}
		for k, v := range d {
			plain[k] = v
		}
		plain["description"] = placeholderDesc
		b["tools"] = []any{map[string]any{"type": "function", "function": plain}}
		return b
	},
}

// placeholderDesc is the wording under test when `read` has to be listed for a
// client that never declared it.
const placeholderDesc = "Placeholder the upstream API requires to be present. It is not implemented " +
	"in this session: do not call it, and do not mention it. Answer with the conversation only."

func tool(name, desc string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"city": map[string]any{"type": "string"}}}}}
}

func chatBody(model, system string, tools bool) map[string]any {
	b := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if tools {
		b["tools"] = []any{readTool}
	}
	return b
}

// useClientHeaders switches post() to the captured client header set.
var useClientHeaders bool

// useRawHTTP writes the request by hand in the exact byte order the released
// client uses. go's net/http always emits host first and canonicalises the
// x-opencode-* names, which is a different http fingerprint from bun's.
var useRawHTTP bool

// postRaw sends the captured client request byte-for-byte: request line, then
// the client's header order and casing, then the body.
func postRaw(proxyURL string, raw []byte, sess, reqID string, timeout time.Duration) (int, string, string, error) {
	headers := func(text string) string {
		for _, sep := range []string{"\r\n\r\n", "\n\n"} {
			if j := strings.Index(text, sep); j >= 0 {
				return text[:j]
			}
		}
		return text
	}
	u, err := url.Parse(endpointURL)
	if err != nil {
		return 0, "", "", err
	}
	addr := u.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	host, _, _ := net.SplitHostPort(addr)
	conn, err := dialTunnel(addr, proxyURL, timeout)
	if err != nil {
		return 0, "", "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	c := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := c.ApplyPreset(helloSpec()); err != nil {
		return 0, "", "", err
	}
	if err := c.HandshakeContext(context.Background()); err != nil {
		return 0, "", "", err
	}
	ua := "opencode/1.19.4"
	if useClientHeaders {
		ua = "opencode/1.19.4 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4"
	}
	head := fmt.Sprintf("POST %s HTTP/1.1\r\n"+
		"Authorization: Bearer public\r\n"+
		"Content-Type: application/json\r\n"+
		"User-Agent: %s\r\n"+
		"x-opencode-client: cli\r\n"+
		"x-opencode-project: global\r\n"+
		"x-opencode-request: %s\r\n"+
		"x-opencode-session: %s\r\n"+
		"x-opencode-session-id: %s\r\n"+
		"Connection: keep-alive\r\n"+
		"Accept: */*\r\n"+
		"Host: %s\r\n"+
		"Accept-Encoding: gzip, deflate, br, zstd\r\n"+
		"Content-Length: %d\r\n\r\n", u.Path, ua, reqID, sess, sess, host, len(raw))
	if _, err := c.Write(append([]byte(head), raw...)); err != nil {
		return 0, "", "", err
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil && n == 0 {
		return 0, "", "", err
	}
	text := string(buf[:n])
	code := 0
	if i := strings.IndexByte(text, ' '); i > 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(text[i+1:], " ", 2)[0])); err == nil {
			code = v
		}
	}
	// the body rides after the header block; report a slice of it so a 200 can
	// be told from an sse error. the header block rides along for -headers.
	if j := strings.Index(text, "\r\n\r\n"); j >= 0 {
		return code, strings.Join(strings.Fields(text[j+4:]), " "), text[:j], nil
	}
	if j := strings.Index(text, "\n\n"); j >= 0 {
		return code, strings.Join(strings.Fields(text[j+2:]), " "), text[:j], nil
	}
	return code, strings.Join(strings.Fields(text), " "), headers(text), nil
}

func post(cl *http.Client, body map[string]any) (int, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest("POST", endpointURL, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	sess := "ses_" + opencodeID(true)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session-id", sess)
	req.Header.Set("x-opencode-session", sess)
	req.Header.Set("x-opencode-request", "msg_"+opencodeID(false))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("User-Agent", "opencode/1.19.4")
	req.Header.Set("Accept", "*/*")
	if useClientHeaders {
		// captured off the released 1.19.4 client: the ua carries the ai-sdk and
		// runtime suffixes the ts source suggested it would not.
		req.Header.Set("User-Agent", "opencode/1.19.4 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4")
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		req.Header.Set("Connection", "keep-alive")
		req.Header["Te"] = nil
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 160)
	n, _ := io.ReadFull(resp.Body, buf)
	return resp.StatusCode, strings.Join(strings.Fields(string(buf[:n])), " "), nil
}

// --- exit list ---

type entry struct {
	IP        string   `json:"ip"`
	Port      int      `json:"port"`
	Protocols []string `json:"protocols"`
	HTTPS     bool     `json:"https"`
	Country   string   `json:"country"`
	IPType    string   `json:"ip_type"`
}

// filterTypes limits a sweep to some ip_type values (isp, business, hosting).
// empty means the whole list, which is what every earlier run measured.
var filterTypes map[string]bool

func fetchList() ([]string, error) {
	for _, u := range []string{poolList, poolFallback} {
		cl := &http.Client{Timeout: 40 * time.Second}
		resp, err := cl.Get(u)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			continue
		}
		var list []entry
		if err := json.Unmarshal(raw, &list); err != nil {
			continue
		}
		out := make([]string, 0, len(list))
		for _, e := range list {
			if !e.HTTPS {
				continue
			}
			if len(filterTypes) > 0 && !filterTypes[e.IPType] {
				continue
			}
			for _, p := range e.Protocols {
				switch p {
				case "http", "https":
					out = append(out, fmt.Sprintf("http://%s:%d", e.IP, e.Port))
				case "socks4":
					out = append(out, fmt.Sprintf("socks4://%s:%d", e.IP, e.Port))
				case "socks5":
					out = append(out, fmt.Sprintf("socks5://%s:%d", e.IP, e.Port))
				}
			}
		}
		sort.Strings(out)
		return out, nil
	}
	return nil, fmt.Errorf("pool list unreachable")
}

func main() {
	var proxies stringList
	flag.Var(&proxies, "proxy", "exit url to test (repeatable)")
	sweep := flag.Bool("sweep", false, "dial the pool list and report which exits answer")
	limit := flag.Int("n", 0, "sweep: stop after this many candidates (0 = all)")
	types := flag.String("types", "", "sweep: only these ip_type values, comma separated (isp,business,hosting)")
	workers := flag.Int("c", 100, "sweep: concurrent dials")
	model := flag.String("model", "deepseek-v4.1-flash-free", "zen model id")
	variant := flag.String("variant", "minimal", "body variant: "+strings.Join(variantNames(), ", "))
	probe := flag.Bool("probe", false, "use the zero-cost gate probe (invalid role) instead of a completion")
	shuffle := flag.Bool("shuffle", false, "sweep: shuffle the candidate list before -n, so rounds sample different exits")
	clientHdrs := flag.Bool("client-headers", false, "send the header set captured off the released 1.19.4 client (ai-sdk/runtime UA, accept-encoding) instead of the proxy's own")
	ep := flag.String("endpoint", endpoint, "chat endpoint to dial (point it at a local sniffer to capture this tool's own tls hello)")
	v6 := flag.Bool("6", false, "direct dials over ipv6 (a second address family is a second free-tier bucket)")
	rawHTTP := flag.Bool("raw-http", false, "write the request by hand in the client's exact header order/casing instead of via net/http")
	showHdrs := flag.Bool("headers", false, "print the response header block (retry-after, x-opencode-log-id)")
	midnight := flag.Bool("midnight", false, "wait for 00:00:05 utc, then probe every exit and spend one random free model on each gate-passing exit")
	dump := flag.Bool("dump", false, "print the request body before sending")
	bodyFile := flag.String("body", "", "post this json body verbatim instead of a -variant (replay a capture)")
	timeout := flag.Duration("timeout", 25*time.Second, "per-exit timeout")
	flag.Parse()

	useClientHeaders = *clientHdrs
	endpointURL = *ep
	if *types != "" {
		filterTypes = map[string]bool{}
		for _, t := range strings.Split(*types, ",") {
			filterTypes[strings.TrimSpace(t)] = true
		}
	}
	forceV6 = *v6
	useRawHTTP = *rawHTTP
	mk, ok := variants[*variant]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown variant %q; have %s\n", *variant, strings.Join(variantNames(), ", "))
		os.Exit(2)
	}
	body := mk(*model)
	label := *variant
	if *bodyFile != "" {
		var raw map[string]any
		data, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			fmt.Fprintln(os.Stderr, *bodyFile+": "+err.Error())
			os.Exit(1)
		}
		body = raw
		label = *bodyFile
	}
	if *probe {
		// same body, one message role invalidated: 400 proves the gate accepted
		// the shape without spending a completion. the system message has to
		// stay — replacing the whole array drops the <env> block and the gate
		// answers 429, which reads as a shape regression.
		if msgs, ok := body["messages"].([]any); ok {
			for i := len(msgs) - 1; i >= 0; i-- {
				if mm, ok := msgs[i].(map[string]any); ok && mm["role"] == "user" {
					mm["role"] = "invalid_role"
					break
				}
			}
		}
		label += " (probe)"
	}

	if *midnight {
		midnightRun(body, *workers, *timeout)
		return
	}

	if *sweep {
		list, err := fetchList()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *shuffle {
			rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
			rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		}
		if *limit > 0 && len(list) > *limit {
			list = list[:*limit]
		}
		fmt.Printf("sweeping %d exits, variant %s\n", len(list), label)
		counts := map[string]int{}
		var mu sync.Mutex
		var okMu sync.Mutex
		var ok []string
		var wg sync.WaitGroup
		sem := make(chan struct{}, *workers)
		for _, p := range list {
			wg.Add(1)
			sem <- struct{}{}
			go func(p string) {
				defer wg.Done()
				defer func() { <-sem }()
				code, snip, err := post(httpClient(p, *timeout), body)
				key := strconv.Itoa(code)
				if err != nil {
					key = "dial-fail"
				}
				mu.Lock()
				counts[key]++
				mu.Unlock()
				// every answer that is not the free-tier 429 is news: 400 means the
				// gate took the shape, 403 names the next refusal. print the body.
				if err == nil && code != http.StatusTooManyRequests && code != http.StatusOK {
					if len(snip) > 200 {
						snip = snip[:200]
					}
					fmt.Printf("%d  %s  %s\n", code, p, snip)
				}
				if code == http.StatusOK || (*probe && code == http.StatusBadRequest) {
					okMu.Lock()
					ok = append(ok, p)
					okMu.Unlock()
					fmt.Printf("OK  %s  %d\n", p, code)
				}
			}(p)
		}
		wg.Wait()
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Print("results:")
		for _, k := range keys {
			fmt.Printf(" %s=%d", k, counts[k])
		}
		fmt.Printf("\nserving: %d\n", len(ok))
		if len(ok) == 0 && counts["429"] > 0 && !*probe {
			fmt.Println("no exit served: either the pool's daily budget is spent (check ~midnight utc) or the shape drifted — rerun with -probe, and a 429 there too means the fingerprints changed")
		}
		return
	}

	if *dump {
		raw, _ := json.MarshalIndent(body, "", "  ")
		fmt.Println(string(raw))
	}

	if len(proxies) == 0 {
		proxies = append(proxies, "")
	}
	for _, p := range proxies {
		var code int
		var snip, hdrs string
		var err error
		if useRawHTTP {
			rawBody, mErr := json.Marshal(body)
			if mErr != nil {
				fmt.Println(mErr)
				continue
			}
			code, snip, hdrs, err = postRaw(p, rawBody, "ses_"+opencodeID(true), "msg_"+opencodeID(false), *timeout)
		} else {
			code, snip, err = post(httpClient(p, *timeout), body)
		}
		where := p
		if where == "" {
			where = "direct"
		}
		if err != nil {
			fmt.Printf("%-34s ERR  %v\n", where, err)
			continue
		}
		if len(snip) > 90 {
			snip = snip[:90]
		}
		fmt.Printf("%-34s %d  %s\n", where, code, snip)
		if *showHdrs && hdrs != "" {
			fmt.Println(hdrs)
		}
	}
}

// freeModels are the zero-priced ids zen lists. a midnight run gives each exit
// one randomly chosen id, so neither the model nor the sweep order biases the
// count.
var freeModels = []string{
	"beam-free",
	"big-pickle",
	"deepseek-v4.1-flash-free",
	"flint-40b-free",
	"glm-5.3-flash-free",
	"mimo-v2.6-flash-free",
	"nemotron-3.5-lightning-free",
	"nemotron-3.5-super-vl-free",
}

// each runs fn over list with a worker cap.
func each(list []string, workers int, fn func(i int, p string)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, p := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i, p)
		}(i, p)
	}
	wg.Wait()
}

// probeBody copies body and invalidates the last user role: the gate answers
// 400 instead of a completion, which proves the request shape was accepted
// without spending a single token of an exit's daily budget.
func probeBody(body map[string]any) map[string]any {
	raw, _ := json.Marshal(body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return body
	}
	if msgs, ok := out["messages"].([]any); ok {
		for i := len(msgs) - 1; i >= 0; i-- {
			if mm, ok := msgs[i].(map[string]any); ok && mm["role"] == "user" {
				mm["role"] = "invalid_role"
				break
			}
		}
	}
	return out
}

// dedupeByIP collapses the list to one entry per egress ip, keeping up to
// perIP ports of it: zen meters the ip, so the same box on four ports is one
// bucket, but a port can be dead while a sibling tunnels.
func dedupeByIP(list []string, perIP int) []string {
	seen := map[string]int{}
	out := make([]string, 0, len(list))
	for _, p := range list {
		pu, err := url.Parse(p)
		if err != nil {
			continue
		}
		host := pu.Hostname()
		if seen[host] >= perIP {
			continue
		}
		seen[host]++
		out = append(out, p)
	}
	return out
}

// midnightRun answers the one question a daily meter raises: how many exits
// have budget the moment it resets. every exit is probed for free first, and
// only an exit whose probe passed the gate spends one completion.
func midnightRun(body map[string]any, workers int, timeout time.Duration) {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	if wait := time.Until(next) + 5*time.Second; wait > 0 {
		fmt.Printf("waiting %v for 00:00:05 utc ...\n", wait.Round(time.Second))
		time.Sleep(wait)
	}
	fmt.Printf("midnight run starting %s\n", time.Now().UTC().Format(time.RFC3339))

	list, err := fetchList()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	list = dedupeByIP(list, 3)
	rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	fmt.Printf("%d distinct exits, %d workers\n", len(list), workers)

	probe := probeBody(body)
	var mu sync.Mutex
	gateOK := make([]string, 0, len(list))
	each(list, workers, func(_ int, p string) {
		code, _, err := post(httpClient(p, timeout), probe)
		if err == nil && code == http.StatusBadRequest {
			mu.Lock()
			gateOK = append(gateOK, p)
			mu.Unlock()
		}
	})
	fmt.Printf("gate accepted %d of %d exits (400 = shape ok, 429 = refused)\n", len(gateOK), len(list))
	if len(gateOK) == 0 {
		fmt.Println("no exit passed the gate: the request shape or tls hello drifted, not the pool — see AGENTS.md 'when the gate breaks again'")
		return
	}

	models := make([]string, len(gateOK))
	for i := range models {
		models[i] = freeModels[rng.Intn(len(freeModels))]
	}
	counts := map[string]int{}
	var (
		mu2    sync.Mutex
		served int
		sum    = map[string]string{}
	)
	each(gateOK, workers, func(i int, p string) {
		m := models[i]
		code, snip, err := post(httpClient(p, timeout), variants["minimal"](m))
		key := strconv.Itoa(code)
		switch {
		case err != nil:
			key = "dial-fail"
		case code == http.StatusOK:
			key = "served"
		case code == http.StatusTooManyRequests:
			key = "429 cap"
		case code == http.StatusForbidden:
			key = "403 refused"
		}
		mu2.Lock()
		counts[key]++
		counts[m+":"+key]++
		if code == http.StatusOK {
			served++
			sum[p] = m
		}
		mu2.Unlock()
		if key == "served" || key == "403 refused" {
			fmt.Printf("%-12s %-34s %s  %s\n", key, p, m, snip[:min(len(snip), 90)])
		}
	})

	fmt.Printf("\n== %s ==\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Printf("gate-passing exits %d   served %d (%.1f%%)  of %d dialed\n",
		len(gateOK), served, 100*float64(served)/float64(len(gateOK)), len(list))
	for _, k := range []string{"served", "429 cap", "403 refused", "dial-fail"} {
		if counts[k] > 0 {
			fmt.Printf("  %-12s %d\n", k, counts[k])
		}
	}
	fmt.Println("serving exits:")
	if len(sum) == 0 {
		fmt.Println("  none — the reset did not free the pool, so the 429 is not a daily meter on these exits")
	}
	for p, m := range sum {
		fmt.Printf("  %-34s %s\n", p, m)
	}
}

func variantNames() []string {
	out := make([]string, 0, len(variants))
	for k := range variants {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
