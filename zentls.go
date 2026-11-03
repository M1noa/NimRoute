// zentls.go — the TLS half of zen's free-tier client gate.
//
// the gate fingerprints the ClientHello as well as the body: measured
// 2026-10-26 on one exit ip, minutes apart, requests byte-identical in every
// other respect —
//
//	go crypto/tls          -> 429 FreeUsageLimitError
//	utls + this spec       -> 200 (a real completion)
//
// the accepted hello is the one bun's fetch sends, captured off the wire from
// the released client (record 1603 01, no GREASE, BoringSSL extension order,
// ALPN http/1.1 only, hybrid X25519MLKEM768 key share). go's stack differs in
// the extension set and order, which is why a correct body alone still 429'd.
package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

// zenCipherSuites is the client's cipher list, in its order: the three tls1.3
// suites, then BoringSSL's tls1.2 preference including the RSA-kex suites go
// never offers by default.
var zenCipherSuites = []uint16{
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

// zentlsSpec rebuilds the captured hello. the extension order, the SNI-first
// position, the single http/1.1 ALPN entry and the two key shares (hybrid
// first) are all part of what the gate reads: changing any of them is what
// turned the handshake into a 429 or a refusal.
func zentlsSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       zenCipherSuites,
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
				0x0403, // ecdsa_secp256r1_sha256
				0x0804, // rsa_pss_rsae_sha256
				0x0401, // rsa_pkcs1_sha256
				0x0503, // ecdsa_secp384r1_sha384
				0x0805, // rsa_pss_rsae_sha384
				0x0501, // rsa_pkcs1_sha384
				0x0806, // rsa_pss_rsae_sha512
				0x0601, // ed25519
				0x0201, // rsa_pkcs1_sha1
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

// zenTLS wraps an established tunnel in the client's TLS hello.
func zenTLS(ctx context.Context, raw net.Conn, host string, timeout time.Duration) (net.Conn, error) {
	raw.SetDeadline(time.Now().Add(timeout))
	c := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := c.ApplyPreset(zentlsSpec()); err != nil {
		raw.Close()
		return nil, err
	}
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	return c, nil
}

// zenDialTLS is the zen transports' DialTLSContext: it owns the whole connect
// (direct, http CONNECT, or socks tunnel) and finishes it with the client's
// hello. plain DialContext cannot be used for this — the transport would then
// wrap the tunnel with crypto/tls and lose the fingerprint.
func zenDialTLS(proxyURL string, pu *url.URL, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var raw net.Conn
		switch pu.Scheme {
		case "socks4":
			raw, err = socks4Dial(ctx, network, addr, pu.Host)
		case "socks5":
			raw, err = socks5Dial(ctx, network, addr, pu)
		default:
			if proxyURL == "" {
				// direct: no exit to tunnel through, dial the upstream itself.
				// antDialConnect builds a CONNECT request to proxyURL and fails
				// with "missing address" when it is empty, which broke every
				// send that ran without a pin.
				d := net.Dialer{Timeout: timeout}
				raw, err = d.DialContext(ctx, network, addr)
			} else {
				raw, err = antDialConnect(addr, proxyURL, timeout)
			}
		}
		if err != nil {
			return nil, err
		}
		return zenTLS(ctx, raw, host, timeout)
	}
}
