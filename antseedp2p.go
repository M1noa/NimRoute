// antseed p2p wire: identity, handshake, frames, free-usage signing.
// mirrors @antseed/protocol (signing, framing, request-codec, connection-auth),
// secure-channel.ts, and buyer-free-usage-manager.ts. free offers only:
// identity is minted fresh in memory per process run and never saved.

package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/sha3"
)

// antseed message types (packages/protocol/src/messages.ts).
const (
	antMsgPing            = 0x10
	antMsgPong            = 0x11
	antMsgHTTPRequest     = 0x20
	antMsgHTTPResponse    = 0x21
	antMsgHTTPChunk       = 0x22
	antMsgHTTPEnd         = 0x23
	antMsgHTTPError       = 0x24
	antMsgHTTPReqChunk    = 0x25
	antMsgHTTPReqEnd      = 0x26
	antMsgFreeUsageOpen   = 0x52
	antMsgFreeUsageAuth   = 0x53
	antMsgFreeUsageAck    = 0x54
	antMsgNeedFreeUsage   = 0x55
	antMsgPaymentRequired = 0x56
	antMsgNeedAuth        = 0x58
)

const (
	antFrameHeader = 9
	antMaxPlain    = 16 << 20
	antUploadChunk = 8 << 10
	antUploadLimit = 240 << 10
	antHKDFInfo    = "antseed-tcp-enc-v1"
	antMsgDomain   = "antseed-msg-v1:"
	antChainID     = 8453
	antFreeUsageSC = "0xB24DcDE777C4ad9AC385Ec9500b3Dc527a4F91Ad"
)

var antLocalCaps = []string{
	"verification.response-auth.v1",
	"payments.cooperative-close.v1",
	"transport.signed-sdp.v1",
	"transport.tcp-enc.v1",
}

func antKeccak(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}

// antseedIdentity is the buyer key: fresh secp256k1 per process run,
// rotated on 429. never persisted (privacy: no stable id across runs).
type antseedIdentity struct {
	priv []byte // 32b secp256k1 scalar
	addr string // 0x + 40hex evm address
}

var (
	antIDMu sync.Mutex
	antID   *antseedIdentity
)

func antIdentity() *antseedIdentity {
	antIDMu.Lock()
	defer antIDMu.Unlock()
	if antID == nil {
		antID = antMintIdentity()
	}
	return antID
}

// antRotateIdentity mints a fresh buyer id; called on 429 failover.
func antRotateIdentity() *antseedIdentity {
	nid := antMintIdentity()
	antIDMu.Lock()
	antID = nid
	antIDMu.Unlock()
	return nid
}

func antMintIdentity() *antseedIdentity {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		// crypto/rand failure means the host entropy is broken; without
		// key material there is nothing safe to do. log and exit instead
		// of panicking through the http handler.
		log.Fatalf("antseed: crypto/rand failed: %v", err)
	}
	var scalar secp256k1.ModNScalar
	scalar.SetByteSlice(priv)
	// SetByteSlice reduces mod N: zero means priv was 0 (degenerate key),
	// non-zero is always a valid scalar, so no order comparison exists to do.
	if scalar.IsZero() {
		return antMintIdentity()
	}
	pk := secp256k1.PrivKeyFromBytes(priv)
	uncomp := pk.PubKey().SerializeUncompressed() // 0x04 || x || y
	addr := antKeccak(uncomp[1:])
	return &antseedIdentity{priv: priv, addr: "0x" + hex.EncodeToString(addr[12:])}
}

func antPrivKey(id *antseedIdentity) *secp256k1.PrivateKey {
	return secp256k1.PrivKeyFromBytes(id.priv)
}

// antEthHash is the eip-191 personal_sign hash antSignMsg signs.
func antEthHash(msg []byte) []byte {
	pre := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))
	return antKeccak(append([]byte(pre), msg...))
}

// antSignMsg is ethers personal_sign over msg, eth r||s||v format.
func antSignMsg(id *antseedIdentity, msg []byte) []byte {
	h := antEthHash(msg)
	c := ecdsa.SignCompact(antPrivKey(id), h, false) // 27/28 || r || s
	out := make([]byte, 65)
	copy(out, c[1:])
	out[64] = c[0]
	return out
}

// antRecoverAddr recovers the 0x signer of an eth-format sig over hash.
func antRecoverAddr(hash, sig []byte) (string, bool) {
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		return "", false
	}
	c := make([]byte, 65)
	c[0] = sig[64]
	copy(c[1:], sig[:64])
	pub, _, err := ecdsa.RecoverCompact(c, hash)
	if err != nil {
		return "", false
	}
	uncomp := pub.SerializeUncompressed()
	addr := antKeccak(uncomp[1:])
	return "0x" + hex.EncodeToString(addr[12:]), true
}

// --- eip-712 ---

func antABIUint(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func antABIAddr(a string) []byte {
	a = strings.TrimPrefix(strings.ToLower(a), "0x")
	b, _ := hex.DecodeString(a)
	out := make([]byte, 32)
	copy(out[12:], b)
	return out
}

func antABIBytes32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out, b)
	return out
}

func antABIString(s string) []byte { return antKeccak([]byte(s)) }

func antEIP712Domain() []byte {
	// keccak("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	typeHash := antKeccak([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	enc := append(typeHash,
		append(antABIString("AntseedFreeUsage"), antABIString("1")...)...)
	enc = append(enc, antABIUint(big.NewInt(antChainID))...)
	enc = append(enc, antABIAddr(antFreeUsageSC)...)
	return antKeccak(enc)
}

func antSignTyped(id *antseedIdentity, typeHash, enc []byte) []byte {
	digest := antKeccak(append([]byte("\x19\x01"), append(antEIP712Domain(), antKeccak(append(typeHash, enc...))...)...))
	// signTypedData signs the raw eip-712 digest, no eth prefix.
	c := ecdsa.SignCompact(antPrivKey(id), digest, false)
	out := make([]byte, 65)
	copy(out, c[1:])
	out[64] = c[0]
	return out
}

var (
	antFreeOpenType = antKeccak([]byte("FreeUsageOpen(bytes32 channelId,uint256 deadline)"))
	antFreeAuthType = antKeccak([]byte("FreeUsageAuth(bytes32 channelId,uint256 sequence,bytes32 metadataHash,uint256 deadline)"))
	antFreeDomainID = antKeccak([]byte("ANTSEED_FREE_USAGE_CHANNEL"))
)

// antChannelID = keccak(abi(bytes32 domain, address buyer, address seller, bytes32 salt)).
func antChannelID(buyer, seller string, salt []byte) []byte {
	enc := append(antABIBytes32(antFreeDomainID), antABIAddr(buyer)...)
	enc = append(enc, antABIAddr(seller)...)
	enc = append(enc, antABIBytes32(salt)...)
	return antKeccak(enc)
}

type antServiceUsage struct {
	service string
	in      uint64
	cached  uint64
	out     uint64
	reqs    uint64
}

// antEncodeFreeMetadata v1: abi(uint256 1, cumIn, cumOut, cumReq,
// v2 services sorted by serviceId). service tuple:
// (bytes32 keccak(service), amount=0, in, cachedIn, out, req).
func antEncodeFreeMetadata(in, out, reqs uint64, svcs []antServiceUsage) []byte {
	type row struct {
		id  []byte
		enc []byte
	}
	var rows []row
	for _, s := range svcs {
		id := antKeccak([]byte(strings.TrimSpace(s.service)))
		e := append(antABIBytes32(id), antABIUint(big.NewInt(0))...)
		e = append(e, antABIUint(new(big.Int).SetUint64(s.in))...)
		e = append(e, antABIUint(new(big.Int).SetUint64(s.cached))...)
		e = append(e, antABIUint(new(big.Int).SetUint64(s.out))...)
		e = append(e, antABIUint(new(big.Int).SetUint64(s.reqs))...)
		rows = append(rows, row{id, e})
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && bytes.Compare(rows[j].id, rows[j-1].id) < 0; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	// dynamic array: head = 5 fixed words, offset 0xA0, then len, then rows.
	head := append(antABIUint(big.NewInt(1)), antABIUint(new(big.Int).SetUint64(in))...)
	head = append(head, antABIUint(new(big.Int).SetUint64(out))...)
	head = append(head, antABIUint(new(big.Int).SetUint64(reqs))...)
	head = append(head, antABIUint(big.NewInt(0xA0))...)
	tail := append(antABIUint(big.NewInt(int64(len(rows)))), []byte{}...)
	for _, r := range rows {
		tail = append(tail, r.enc...)
	}
	return append(head, tail...)
}

func antFreeMetadataHash(in, out, reqs uint64, svcs []antServiceUsage) []byte {
	return antKeccak(antEncodeFreeMetadata(in, out, reqs, svcs))
}

// --- frame codec ---

func antEncodeFrame(typ byte, msgID uint32, payload []byte) []byte {
	out := make([]byte, antFrameHeader+len(payload))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:5], msgID)
	binary.BigEndian.PutUint32(out[5:9], uint32(len(payload)))
	copy(out[9:], payload)
	return out
}

func antDecodeFrame(plain []byte) (byte, uint32, []byte, error) {
	if len(plain) < antFrameHeader {
		return 0, 0, nil, fmt.Errorf("antseed: short frame")
	}
	n := binary.BigEndian.Uint32(plain[5:9])
	if int(n) > len(plain)-antFrameHeader {
		return 0, 0, nil, fmt.Errorf("antseed: frame len mismatch")
	}
	return plain[0], binary.BigEndian.Uint32(plain[1:5]),
		plain[antFrameHeader : antFrameHeader+int(n)], nil
}

// --- http-over-p2p codec (packages/protocol/src/request-codec.ts) ---

type antHTTPReq struct {
	id      string
	method  string
	path    string
	headers map[string]string
	body    []byte
}

func antEncodeHTTPReq(q antHTTPReq) []byte {
	var b bytes.Buffer
	b.Write(uint16be(len(q.id)))
	b.WriteString(q.id)
	b.WriteByte(byte(len(q.method)))
	b.WriteString(q.method)
	b.Write(uint16be(len(q.path)))
	b.WriteString(q.path)
	keys := make([]string, 0, len(q.headers))
	for k := range q.headers {
		keys = append(keys, k)
	}
	b.Write(uint16be(len(keys)))
	for _, k := range keys {
		b.Write(uint16be(len(k)))
		b.WriteString(k)
		v := q.headers[k]
		b.Write(uint16be(len(v)))
		b.WriteString(v)
	}
	b.Write(uint32be(len(q.body)))
	b.Write(q.body)
	return b.Bytes()
}

type antHTTPResp struct {
	id     string
	status int
	header map[string]string
	body   []byte
}

func antDecodeHTTPResp(p []byte) (antHTTPResp, error) {
	var r antHTTPResp
	r.header = map[string]string{}
	if len(p) < 2 {
		return r, fmt.Errorf("antseed: short response")
	}
	n := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	if len(p) < n+2 {
		return r, fmt.Errorf("antseed: short response id")
	}
	r.id = string(p[:n])
	p = p[n:]
	r.status = int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	if len(p) < 2 {
		return r, fmt.Errorf("antseed: short response headers")
	}
	nh := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	for i := 0; i < nh; i++ {
		if len(p) < 2 {
			return r, fmt.Errorf("antseed: short header key")
		}
		kl := int(binary.BigEndian.Uint16(p[:2]))
		p = p[2:]
		if len(p) < kl+2 {
			return r, fmt.Errorf("antseed: short header key data")
		}
		k := string(p[:kl])
		p = p[kl:]
		vl := int(binary.BigEndian.Uint16(p[:2]))
		p = p[2:]
		if len(p) < vl+4 {
			return r, fmt.Errorf("antseed: short header val")
		}
		r.header[strings.ToLower(k)] = string(p[:vl])
		p = p[vl:]
	}
	if len(p) < 4 {
		return r, fmt.Errorf("antseed: short body len")
	}
	bl := int(binary.BigEndian.Uint32(p[:4]))
	p = p[4:]
	if len(p) < bl {
		return r, fmt.Errorf("antseed: short body")
	}
	r.body = p[:bl]
	return r, nil
}

func antDecodeChunk(p []byte) (id string, done bool, data []byte, err error) {
	if len(p) < 2 {
		return "", false, nil, fmt.Errorf("antseed: short chunk")
	}
	n := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	if len(p) < n+1+4 {
		return "", false, nil, fmt.Errorf("antseed: short chunk id")
	}
	id = string(p[:n])
	p = p[n:]
	done = p[0] == 1
	p = p[1:]
	dl := int(binary.BigEndian.Uint32(p[:4]))
	p = p[4:]
	if len(p) < dl {
		return "", false, nil, fmt.Errorf("antseed: short chunk data")
	}
	return id, done, p[:dl], nil
}

// uint16be errors instead of silently truncating: lengths above 64k
// are a caller bug, not something to wrap.
func uint16be(n int) []byte {
	if n < 0 || n > 0xffff {
		panic(fmt.Sprintf("antseed: uint16be overflow %d", n))
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(n))
	return b[:]
}

func uint32be(n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

// --- dialers ---

func antDialProxy(network, addr, proxyURL string, timeout time.Duration) (net.Conn, error) {
	if proxyURL == "" {
		return net.DialTimeout(network, addr, timeout)
	}
	if strings.HasPrefix(proxyURL, "socks5://") || strings.HasPrefix(proxyURL, "socks5h://") {
		return antDialSocks5(addr, proxyURL, timeout)
	}
	return antDialConnect(addr, proxyURL, timeout)
}

func antProxyAuth(pu string) (host string, auth string) {
	u, err := url.Parse(pu)
	if err != nil || u.Host == "" {
		return pu, ""
	}
	if u.User != nil {
		pw, _ := u.User.Password()
		auth = u.User.Username()
		if pw != "" {
			auth += ":" + pw
		}
	}
	return u.Host, auth
}

// antDialConnect tunnels tcp through an http proxy via CONNECT.
func antDialConnect(addr, proxyURL string, timeout time.Duration) (net.Conn, error) {
	host, auth := antProxyAuth(proxyURL)
	c, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.Close()
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if auth != "" {
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n",
			base64.StdEncoding.EncodeToString([]byte(auth)))
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
		return nil, fmt.Errorf("antseed: connect refused %q", strings.TrimSpace(line))
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
	if br.Buffered() > 0 {
		c.Close()
		return nil, fmt.Errorf("antseed: connect left buffered bytes")
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// antDialSocks5: minimal socks5 (no-auth + user/pass), stdlib only.
func antDialSocks5(addr, proxyURL string, timeout time.Duration) (net.Conn, error) {
	host, auth := antProxyAuth(proxyURL)
	c, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.Close()
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("antseed: bad addr %q: %v", addr, err)
	}
	hp := []string{host, portStr}
	methods := []byte{0x00}
	if auth != "" {
		methods = []byte{0x00, 0x02}
	}
	if _, err := c.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		c.Close()
		return nil, err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		return nil, err
	}
	if rep[0] != 0x05 {
		c.Close()
		return nil, fmt.Errorf("antseed: bad socks version")
	}
	if rep[1] == 0x02 {
		up := strings.SplitN(auth, ":", 2)
		u, pw := up[0], ""
		if len(up) > 1 {
			pw = up[1]
		}
		var b bytes.Buffer
		b.WriteByte(0x01)
		b.WriteByte(byte(len(u)))
		b.WriteString(u)
		b.WriteByte(byte(len(pw)))
		b.WriteString(pw)
		if _, err := c.Write(b.Bytes()); err != nil {
			c.Close()
			return nil, err
		}
		st := make([]byte, 2)
		if _, err := io.ReadFull(c, st); err != nil {
			c.Close()
			return nil, err
		}
		if st[1] != 0x00 {
			c.Close()
			return nil, fmt.Errorf("antseed: socks auth failed")
		}
	} else if rep[1] != 0x00 {
		c.Close()
		return nil, fmt.Errorf("antseed: socks needs auth 0x%02x", rep[1])
	}
	var req bytes.Buffer
	req.WriteByte(0x05)
	req.WriteByte(0x01)
	req.WriteByte(0x00)
	if ip := net.ParseIP(hp[0]); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req.WriteByte(0x01)
			req.Write(v4)
		} else {
			req.WriteByte(0x04)
			req.Write(ip.To16())
		}
	} else {
		req.WriteByte(0x03)
		req.WriteByte(byte(len(hp[0])))
		req.WriteString(hp[0])
	}
	port, err := strconv.Atoi(hp[1])
	if err != nil || port < 0 || port > 65535 {
		c.Close()
		return nil, fmt.Errorf("antseed: bad port %q", hp[1])
	}
	req.WriteByte(byte(port >> 8))
	req.WriteByte(byte(port))
	if _, err := c.Write(req.Bytes()); err != nil {
		c.Close()
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		c.Close()
		return nil, err
	}
	if hdr[1] != 0x00 {
		c.Close()
		return nil, fmt.Errorf("antseed: socks connect failed 0x%02x", hdr[1])
	}
	switch hdr[3] {
	case 0x01:
		io.CopyN(io.Discard, c, 4+2)
	case 0x03:
		var l [1]byte
		io.ReadFull(c, l[:])
		io.CopyN(io.Discard, c, int64(l[0])+2)
	case 0x04:
		io.CopyN(io.Discard, c, 16+2)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// --- connection ---

type antFrameMsg struct {
	typ     byte
	payload []byte
}

// antConn is one encrypted seller connection: multiplexed by requestId.
type antConn struct {
	peerID string
	conn   net.Conn
	send   cipher.AEAD
	recv   cipher.AEAD
	sendN  uint64
	recvN  uint64
	msgID  uint32
	wmu    sync.Mutex
	mu     sync.Mutex
	pend   map[string]chan antFrameMsg
	free   *antFreeSession
	closed chan struct{}
	// lastUse is the last request served: conns idle past the reap
	// window are closed instead of held until fd exhaustion.
	lastUse atomic.Int64 // unix nanos
}

type antFreeSession struct {
	mu       sync.Mutex
	chID     []byte
	chHex    string
	salt     []byte
	deadline int64
	seq      uint64
	in       uint64
	out      uint64
	reqs     uint64
	bySvc    map[string]*antServiceUsage
}

// decHex decodes peer-supplied hex without panicking: a hostile seller
// signs non-hex pub values to crash the dial path.
func decHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func antIV(n uint64) []byte {
	iv := make([]byte, 12)
	binary.BigEndian.PutUint64(iv[4:], n)
	return iv
}

// antOpen connects + handshakes with a seller at hostPort.
func antOpen(peerID, hostPort, proxyURL string, timeout time.Duration) (*antConn, error) {
	raw, err := antDialProxy("tcp", hostPort, proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	id := antIdentity()
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		raw.Close()
		return nil, err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		raw.Close()
		return nil, err
	}
	pubHex := hex.EncodeToString(pub)
	ts := time.Now().UnixMilli()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		raw.Close()
		return nil, err
	}
	nonceHex := hex.EncodeToString(nonce)
	capsJSON, _ := json.Marshal(antLocalCaps)
	myID := strings.ToLower(strings.TrimPrefix(id.addr, "0x"))
	introPayload := fmt.Sprintf("intro|%s|%d|%s|caps=%s|enc=%s",
		myID, ts, nonceHex, capsJSON, pubHex)
	introSig := hex.EncodeToString(antSignMsg(id, []byte(antMsgDomain+introPayload)))
	encSig := hex.EncodeToString(antSignMsg(id, []byte(antMsgDomain+
		fmt.Sprintf("enc-init|%s|%d|%s|%s", myID, ts, nonceHex, pubHex))))
	line, _ := json.Marshal(map[string]any{
		"type": "intro",
		"auth": map[string]any{
			"peerId": myID,
			"ts":     ts,
			"nonce":  nonceHex,
			"sig":    introSig,
			"v":      2,
		},
		"capabilities": antLocalCaps,
		"enc":          map[string]any{"pub": pubHex, "sig": encSig},
	})
	line = append(line, '\n')
	raw.SetDeadline(time.Now().Add(timeout))
	if _, err := raw.Write(line); err != nil {
		raw.Close()
		return nil, err
	}
	var ackLine bytes.Buffer
	one := make([]byte, 1)
	for {
		if _, err := raw.Read(one); err != nil {
			raw.Close()
			return nil, err
		}
		if one[0] == '\n' {
			break
		}
		ackLine.WriteByte(one[0])
		if ackLine.Len() > 1<<20 {
			raw.Close()
			return nil, fmt.Errorf("antseed: ack line too long")
		}
	}
	var ack struct {
		Type   string `json:"type"`
		PeerID string `json:"peerId"`
		Ts     int64  `json:"ts"`
		Nonce  string `json:"nonce"`
		Pub    string `json:"pub"`
		Sig    string `json:"sig"`
	}
	if err := json.Unmarshal(ackLine.Bytes(), &ack); err != nil {
		raw.Close()
		return nil, err
	}
	if ack.Type != "enc-ack" || !strings.EqualFold(ack.PeerID, peerID) {
		raw.Close()
		return nil, fmt.Errorf("antseed: bad enc-ack from %s", peerID)
	}
	ackSig, err := hex.DecodeString(strings.TrimPrefix(ack.Sig, "0x"))
	if err != nil {
		raw.Close()
		return nil, err
	}
	ackPayload := fmt.Sprintf("enc-ack|%s|%d|%s|%s|%s",
		strings.ToLower(ack.PeerID), ack.Ts, ack.Nonce, ack.Pub, nonceHex)
	ackHash := antEthHash(append([]byte(antMsgDomain), ackPayload...))
	// verify by recovery: signer must be the dialed peer.
	signer, ok := antRecoverAddr(ackHash, ackSig)
	if !ok || !strings.EqualFold(strings.TrimPrefix(signer, "0x"), peerID) {
		raw.Close()
		return nil, fmt.Errorf("antseed: enc-ack sig not from %s", peerID)
	}
	// docs require ±30s freshness on the ack timestamp; the signature
	// covers a fresh per-dial nonce, so this is defense in depth.
	if dt := time.Now().UnixMilli() - ack.Ts; dt < 0 {
		if -dt > 30000 {
			raw.Close()
			return nil, fmt.Errorf("antseed: enc-ack from the future")
		}
	} else if dt > 30000 {
		raw.Close()
		return nil, fmt.Errorf("antseed: stale enc-ack")
	}
	peerPub, err := decHex(ack.Pub)
	if err != nil || len(peerPub) != 32 {
		raw.Close()
		return nil, fmt.Errorf("antseed: bad enc-ack pub")
	}
	shared, err := curve25519.X25519(priv, peerPub)
	if err != nil {
		raw.Close()
		return nil, err
	}
	tr := fmt.Sprintf("%s|%s|%s|%s|%s|%s",
		myID, strings.ToLower(peerID),
		nonceHex, strings.ToLower(ack.Nonce), pubHex, strings.ToLower(ack.Pub))
	okm := make([]byte, 64)
	salt := sha256.Sum256([]byte(tr))
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, salt[:], []byte(antHKDFInfo)), okm); err != nil {
		raw.Close()
		return nil, err
	}
	sendKey, _ := aes.NewCipher(okm[:32])
	gcmSend, err := cipher.NewGCM(sendKey)
	if err != nil {
		raw.Close()
		return nil, err
	}
	recvKey, _ := aes.NewCipher(okm[32:])
	gcmRecv, err := cipher.NewGCM(recvKey)
	if err != nil {
		raw.Close()
		return nil, err
	}
	ac := &antConn{
		peerID: strings.ToLower(peerID),
		conn:   raw,
		send:   gcmSend,
		recv:   gcmRecv,
		pend:   map[string]chan antFrameMsg{},
		closed: make(chan struct{}),
	}
	raw.SetDeadline(time.Time{})
	go ac.readLoop()
	return ac, nil
}

func (c *antConn) writeRecord(plain []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if len(plain) > antMaxPlain {
		return fmt.Errorf("antseed: plaintext too large")
	}
	ct := c.send.Seal(nil, antIV(c.sendN), plain, nil)
	c.sendN++
	out := append(uint32be(len(ct)), ct...)
	c.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_, err := c.conn.Write(out)
	return err
}

func (c *antConn) writeFrame(typ byte, msgID uint32, payload []byte) error {
	return c.writeRecord(antEncodeFrame(typ, msgID, payload))
}

func (c *antConn) readRecord() ([]byte, error) {
	var ln [4]byte
	if _, err := io.ReadFull(c.conn, ln[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(ln[:])
	if n < 16 || n > antMaxPlain+16 {
		return nil, fmt.Errorf("antseed: bad record len %d", n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(c.conn, ct); err != nil {
		return nil, err
	}
	c.mu.Lock()
	nr := c.recvN
	c.recvN++
	c.mu.Unlock()
	plain, err := c.recv.Open(nil, antIV(nr), ct, nil)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

func (c *antConn) nextMsgID() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgID++
	return c.msgID
}

func (c *antConn) readLoop() {
	defer close(c.closed)
	defer c.conn.Close()
	for {
		plain, err := c.readRecord()
		if err != nil {
			c.failAll()
			return
		}
		typ, _, payload, err := antDecodeFrame(plain)
		if err != nil {
			continue
		}
		switch typ {
		case antMsgPing:
			c.writeFrame(antMsgPong, c.nextMsgID(), payload)
		case antMsgHTTPResponse, antMsgHTTPChunk, antMsgHTTPEnd, antMsgHTTPError:
			c.routeByRequestID(payload, typ)
		case antMsgNeedFreeUsage:
			// inline, not spawned: answerNeedAuth serializes on fs.mu
			// anyway, and a goroutine per peer message is an
			// attacker-controlled fork bomb.
			c.answerNeedAuth(payload)
		case antMsgPaymentRequired, antMsgNeedAuth:
			c.routeByRequestID(payload, typ)
		}
	}
}

// routeByRequestID pulls requestId out of a json payment payload or an
// http response/chunk and delivers to the waiter.
func (c *antConn) routeByRequestID(payload []byte, typ byte) {
	id := antPayloadRequestID(payload, typ)
	if id == "" {
		return
	}
	c.mu.Lock()
	ch, ok := c.pend[id]
	c.mu.Unlock()
	if ok {
		select {
		case ch <- antFrameMsg{typ, payload}:
		default:
		}
	}
}

func antPayloadRequestID(payload []byte, typ byte) string {
	switch typ {
	case antMsgHTTPResponse, antMsgHTTPError:
		r, err := antDecodeHTTPResp(payload)
		if err != nil {
			return ""
		}
		return r.id
	case antMsgHTTPChunk, antMsgHTTPEnd:
		id, _, _, err := antDecodeChunk(payload)
		if err != nil {
			return ""
		}
		return id
	default:
		var m map[string]any
		if json.Unmarshal(payload, &m) != nil {
			return ""
		}
		if s, ok := m["requestId"].(string); ok {
			return s
		}
		return ""
	}
}

func (c *antConn) failAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.pend {
		close(ch)
	}
	c.pend = map[string]chan antFrameMsg{}
}

func (c *antConn) waitFor(id string, timeout time.Duration) ([]antFrameMsg, bool) {
	ch := make(chan antFrameMsg, 64)
	c.mu.Lock()
	c.pend[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pend, id)
		c.mu.Unlock()
	}()
	var out []antFrameMsg
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out, false
			}
			out = append(out, m)
			if m.typ == antMsgHTTPEnd || m.typ == antMsgHTTPResponse || m.typ == antMsgHTTPError {
				return out, true
			}
		case <-t.C:
			return out, false
		case <-c.closed:
			return out, false
		}
	}
}

// antDoRequest sends one http-over-p2p request and reassembles the answer.
// returns status, headers, body.
func (c *antConn) antDoRequest(q antHTTPReq, timeout time.Duration) (int, map[string]string, []byte, error) {
	enc := antEncodeHTTPReq(q)
	if len(q.body) > antUploadLimit {
		// chunked upload: header frame + empty body, then 8k slices.
		// clone the map: mutating the caller's headers is a latent bug.
		hq := q
		hq.body = nil
		hdr := maps.Clone(hq.headers)
		if hdr == nil {
			hdr = map[string]string{}
		}
		hdr["x-antseed-upload"] = "chunked"
		hq.headers = hdr
		if err := c.writeFrame(antMsgHTTPRequest, c.nextMsgID(), antEncodeHTTPReq(hq)); err != nil {
			return 0, nil, nil, err
		}
		for off := 0; off < len(q.body); {
			end := off + antUploadChunk
			if end > len(q.body) {
				end = len(q.body)
			}
			done := byte(0)
			typ := byte(antMsgHTTPReqChunk)
			if end == len(q.body) {
				done = 1
				typ = byte(antMsgHTTPReqEnd)
			}
			cp := append(uint16be(len(q.id)), q.id...)
			cp = append(cp, done)
			cp = append(cp, uint32be(end-off)...)
			cp = append(cp, q.body[off:end]...)
			if err := c.writeFrame(typ, c.nextMsgID(), cp); err != nil {
				return 0, nil, nil, err
			}
			off = end
		}
	} else {
		if err := c.writeFrame(antMsgHTTPRequest, c.nextMsgID(), enc); err != nil {
			return 0, nil, nil, err
		}
	}
	msgs, ok := c.waitFor(q.id, timeout)
	if !ok {
		return 0, nil, nil, fmt.Errorf("antseed: request timeout/no response")
	}
	var body bytes.Buffer
	for _, m := range msgs {
		switch m.typ {
		case antMsgHTTPResponse, antMsgHTTPError:
			r, err := antDecodeHTTPResp(m.payload)
			if err != nil {
				return 0, nil, nil, err
			}
			if m.typ == antMsgHTTPResponse && r.header["x-antseed-streaming"] == "1" {
				continue // stream start marker
			}
			return r.status, r.header, r.body, nil
		case antMsgHTTPChunk:
			_, _, data, err := antDecodeChunk(m.payload)
			if err != nil {
				return 0, nil, nil, err
			}
			body.Write(data)
		case antMsgHTTPEnd:
			_, _, data, err := antDecodeChunk(m.payload)
			if err != nil {
				return 0, nil, nil, err
			}
			body.Write(data)
			return 200, map[string]string{}, body.Bytes(), nil
		case antMsgPaymentRequired, antMsgNeedAuth:
			return 402, map[string]string{}, m.payload, fmt.Errorf("antseed: payment required")
		}
	}
	return 0, nil, nil, fmt.Errorf("antseed: incomplete response")
}

// answerNeedAuth signs cumulative free-usage counters for the channel.
func (c *antConn) answerNeedAuth(payload []byte) {
	var m struct {
		ChannelID string `json:"channelId"`
		Required  string `json:"requiredSequence"`
		In        string `json:"inputTokens"`
		Out       string `json:"outputTokens"`
		Service   string `json:"service"`
	}
	if json.Unmarshal(payload, &m) != nil {
		return
	}
	id := antIdentity()
	c.mu.Lock()
	fs := c.free
	c.mu.Unlock()
	if fs == nil {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !strings.EqualFold(fs.chHex, m.ChannelID) {
		return
	}
	// sequence and counters are peer-supplied: reject garbage instead
	// of saturating into MaxUint64 (which wedges the channel: every
	// later legitimate sequence compares <= fs.seq and is dropped).
	var seq uint64
	if _, err := fmt.Sscanf(m.Required, "%d", &seq); err != nil {
		return
	}
	if seq <= fs.seq || seq > fs.seq+1000000 {
		return
	}
	var in, out uint64
	if _, err := fmt.Sscanf(m.In, "%d", &in); err != nil {
		return
	}
	if _, err := fmt.Sscanf(m.Out, "%d", &out); err != nil {
		return
	}
	// sanity ceiling per message: a single turn never carries more than
	// 10M tokens each way. the peer reports what we consumed, but we
	// sign it as settleable, so unbounded values are refused.
	if in > 10000000 || out > 10000000 {
		return
	}
	fs.in += in
	fs.out += out
	fs.reqs++
	// bySvc is keyed on a raw peer string: cap it, or distinct service
	// names grow the map (and the per-message sort below) forever.
	svc := m.Service
	if u, ok := fs.bySvc[svc]; ok {
		u.in += in
		u.out += out
		u.reqs++
	} else {
		if len(fs.bySvc) >= 256 {
			return
		}
		fs.bySvc[svc] = &antServiceUsage{service: svc, in: in, out: out, reqs: 1}
	}
	var svcs []antServiceUsage
	for _, u := range fs.bySvc {
		svcs = append(svcs, *u)
	}
	// encode once: the hash is over the same bytes we send.
	meta := antEncodeFreeMetadata(fs.in, fs.out, fs.reqs, svcs)
	mh := antKeccak(meta)
	enc := append(antABIBytes32(fs.chID), antABIUint(new(big.Int).SetUint64(seq))...)
	enc = append(enc, antABIBytes32(mh)...)
	enc = append(enc, antABIUint(big.NewInt(fs.deadline))...)
	sig := antSignTyped(id, antFreeAuthType, enc)
	auth, _ := json.Marshal(map[string]any{
		"channelId":              fs.chHex,
		"cumulativeInputTokens":  fmt.Sprint(fs.in),
		"cumulativeOutputTokens": fmt.Sprint(fs.out),
		"sequence":               fmt.Sprint(seq),
		"metadataHash":           "0x" + hex.EncodeToString(mh),
		"metadata":               "0x" + hex.EncodeToString(meta),
		"deadline":               fs.deadline,
		"usageSig":               "0x" + hex.EncodeToString(sig),
	})
	c.writeFrame(antMsgFreeUsageAuth, c.nextMsgID(), auth)
	fs.seq = seq
}

// antOpenFree sends a best-effort FreeUsageOpen for the conn's channel.
func (c *antConn) antOpenFree(sellerAddr string) {
	id := antIdentity()
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return
	}
	chID := antChannelID(id.addr, sellerAddr, salt)
	deadline := time.Now().Unix() + 900
	enc := append(antABIBytes32(chID), antABIUint(big.NewInt(deadline))...)
	sig := antSignTyped(id, antFreeOpenType, enc)
	chHex := "0x" + hex.EncodeToString(chID)
	open, _ := json.Marshal(map[string]any{
		"channelId": chHex,
		"salt":      "0x" + hex.EncodeToString(salt),
		"deadline":  deadline,
		"openSig":   "0x" + hex.EncodeToString(sig),
	})
	c.mu.Lock()
	if c.free != nil && c.free.deadline > time.Now().Unix()+30 {
		c.mu.Unlock()
		return
	}
	c.free = &antFreeSession{
		chID: chID, chHex: chHex, salt: salt, deadline: deadline,
		bySvc: map[string]*antServiceUsage{},
	}
	c.mu.Unlock()
	// failures swallowed: request proceeds anyway (reference behavior).
	c.writeFrame(antMsgFreeUsageOpen, c.nextMsgID(), open)
}
