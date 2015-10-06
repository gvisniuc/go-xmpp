// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO(rsc):
//	More precise error handling.
//	Presence functionality.
// TODO(mattn):
//  Add proxy authentication.

// Package xmpp implements a simple Google Talk client
// using the XMPP protocol described in RFC 3920 and RFC 3921.
package xmpp

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	nsStream  = "http://etherx.jabber.org/streams"
	nsTLS     = "urn:ietf:params:xml:ns:xmpp-tls"
	nsSASL    = "urn:ietf:params:xml:ns:xmpp-sasl"
	nsBind    = "urn:ietf:params:xml:ns:xmpp-bind"
	nsClient  = "jabber:client"
	NsSession = "urn:ietf:params:xml:ns:xmpp-session"
	nsMUC     = "http://jabber.org/protocol/muc"
	nsMUCUser = "http://jabber.org/protocol/muc#user"
)

var DefaultConfig tls.Config

type Cookie uint64

func getCookie() Cookie {
	var buf [8]byte
	if _, err := rand.Reader.Read(buf[:]); err != nil {
		panic("Failed to read random bytes: " + err.Error())
	}
	return Cookie(binary.LittleEndian.Uint64(buf[:]))
}

type Client struct {
	conn   net.Conn // connection to server
	jid    string   // Jabber ID for our connection
	domain string
	p      *xml.Decoder
}

func connect(host, user, passwd string) (net.Conn, error) {
	addr := host

	if strings.TrimSpace(host) == "" {
		a := strings.SplitN(user, "@", 2)
		if len(a) == 2 {
			host = a[1]
		}
	}
	a := strings.SplitN(host, ":", 2)
	if len(a) == 1 {
		host += ":5222"
	}
	proxy := os.Getenv("HTTP_PROXY")
	if proxy == "" {
		proxy = os.Getenv("http_proxy")
	}
	if proxy != "" {
		url, err := url.Parse(proxy)
		if err == nil {
			addr = url.Host
		}
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	if proxy != "" {
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n", host)
		fmt.Fprintf(c, "Host: %s\r\n", host)
		fmt.Fprintf(c, "\r\n")
		br := bufio.NewReader(c)
		req, _ := http.NewRequest("CONNECT", host, nil)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			f := strings.SplitN(resp.Status, " ", 2)
			return nil, errors.New(f[1])
		}
	}
	return c, nil
}

// Options are used to specify additional options for new clients, such as a Resource.
type Options struct {
	// Host specifies what host to connect to, as either "hostname" or "hostname:port"
	// If host is not specified, the  DNS SRV should be used to find the host from the domainpart of the JID.
	// Default the port to 5222.
	Host string

	// User specifies what user to authenticate to the remote server.
	User string

	// Password supplies the password to use for authentication with the remote server.
	Password string

	// Resource specifies an XMPP client resource, like "bot", instead of accepting one
	// from the server.  Use "" to let the server generate one for your client.
	Resource string

	// TLS Config
	TLSConfig *tls.Config

	// InsecureAllowUnencryptedAuth permits authentication over a TCP connection that has not been promoted to
	// TLS by STARTTLS; this could leak authentication information over the network, or permit man in the middle
	// attacks.
	InsecureAllowUnencryptedAuth bool

	// NoTLS directs go-xmpp to not use TLS initially to contact the server; instead, a plain old unencrypted
	// TCP connection should be used. (Can be combined with StartTLS to support STARTTLS-based servers.)
	NoTLS bool

	// StartTLS directs go-xmpp to STARTTLS if the server supports it; go-xmpp will automatically STARTTLS
	// if the server requires it regardless of this option.
	StartTLS bool

	// Debug output
	Debug bool

	// Use server sessions
	Session bool

	// Presence Status
	Status string

	// Status message
	StatusMessage string
}

// NewClient establishes a new Client connection based on a set of Options.
func (o Options) NewClient() (*Client, error) {
	host := o.Host
	c, err := connect(host, o.User, o.Password)
	if err != nil {
		return nil, err
	}

	client := new(Client)
	if o.NoTLS {
		client.conn = c
	} else {
		var tlsconn *tls.Conn
		if o.TLSConfig != nil {
			tlsconn = tls.Client(c, o.TLSConfig)
		} else {
			tlsconn = tls.Client(c, &DefaultConfig)
		}
		if err = tlsconn.Handshake(); err != nil {
			return nil, err
		}
		if strings.LastIndex(o.Host, ":") > 0 {
			host = host[:strings.LastIndex(o.Host, ":")]
		}
		if err = tlsconn.VerifyHostname(host); err != nil {
			return nil, err
		}
		client.conn = tlsconn
	}

	if err := client.init(&o); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

// NewClient creates a new connection to a host given as "hostname" or "hostname:port".
// If host is not specified, the  DNS SRV should be used to find the host from the domainpart of the JID.
// Default the port to 5222.
func NewClient(host, user, passwd string, debug bool) (*Client, error) {
	opts := Options{
		Host:     host,
		User:     user,
		Password: passwd,
		Debug:    debug,
		Session:  false,
	}
	return opts.NewClient()
}

func NewClientNoTLS(host, user, passwd string, debug bool) (*Client, error) {
	opts := Options{
		Host:     host,
		User:     user,
		Password: passwd,
		NoTLS:    true,
		Debug:    debug,
		Session:  false,
	}
	return opts.NewClient()
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// xep-0045 7.2
func (c *Client) JoinMUC(jid string) {
	fmt.Fprintf(c.conn, "<presence to='%s'>\n" +
				"<x xmlns='%s'><history maxstanzas='0'/></x>\n" +
				"</presence>",
		xmlEscape(jid), nsMUC)
}

// xep-0045 7.14
func (c *Client) LeaveMUC(jid string) {
	fmt.Fprintf(c.conn, "<presence from='%s' to='%s' type='unavailable' />",
		c.jid, xmlEscape(jid))
}

// Keep alive (timetout occurs every 150s in hipchat.
func (c *Client) KeepAlive() {
	fmt.Fprintf(c.conn, " ")
}

// Change status when deploying
func (c *Client) ChangeStatus(show string, status string) {
	fmt.Fprintf(c.conn, "<presence xml:lang='en'><show>%s</show><status>%s</status></presence>", xmlEscape(show), xmlEscape(status))

}

func saslDigestResponse(username, realm, passwd, nonce, cnonceStr,
	authenticate, digestUri, nonceCountStr string) string {
	h := func(text string) []byte {
		h := md5.New()
		h.Write([]byte(text))
		return h.Sum(nil)
	}
	hex := func(bytes []byte) string {
		return fmt.Sprintf("%x", bytes)
	}
	kd := func(secret, data string) []byte {
		return h(secret + ":" + data)
	}

	a1 := string(h(username + ":" + realm + ":" + passwd)) + ":" +
			nonce + ":" + cnonceStr
	a2 := authenticate + ":" + digestUri
	response := hex(kd(hex(h(a1)), nonce+":" +
				nonceCountStr+":"+cnonceStr+":auth:" +
				hex(h(a2))))
	return response
}

func cnonce() string {
	randSize := big.NewInt(0)
	randSize.Lsh(big.NewInt(1), 64)
	cn, err := rand.Int(rand.Reader, randSize)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%016x", cn)
}

func (c *Client) init(o *Options) error {
	c.p = xml.NewDecoder(c.conn)
	// For debugging: the following causes the plaintext of the connection to be duplicated to stdout.
	if o.Debug {
		c.p = xml.NewDecoder(tee{c.conn, os.Stdout})
	}

	a := strings.SplitN(o.User, "@", 2)
	if len(a) != 2 {
		return errors.New("xmpp: invalid username (want user@domain): " + o.User)
	}
	domain := a[1]

	// Declare intent to be a jabber client and gather stream features.
	f, err := c.startStream(o, domain)
	if err != nil {
		return err
	}

	// If the server requires we STARTTLS, attempt to do so.
	if f, err = c.startTlsIfRequired(f, o, domain); err != nil {
		return err
	}

	// Even digest forms of authentication are unsafe if we do not know that the host
	// we are talking to is the actual server, and not a man in the middle playing
	// proxy.
	if !c.IsEncrypted() && !o.InsecureAllowUnencryptedAuth {
		return errors.New("refusing to authenticate over unencrypted TCP connection")
	}

	mechanism := ""
	for _, m := range f.Mechanisms.Mechanism {
		if m == "ANONYMOUS" {
			mechanism = m
			fmt.Fprintf(c.conn, "<auth xmlns='%s' mechanism='ANONYMOUS' />\n", nsSASL)
			break
		}

		a := strings.SplitN(o.User, "@", 2)
		if len(a) != 2 {
			return errors.New("xmpp: invalid username (want user@domain): " + o.User)
		}
		user := a[0]
		domain := a[1]

		if m == "PLAIN" {
			mechanism = m
			// Plain authentication: send base64-encoded \x00 user \x00 password.
			raw := "\x00" + user + "\x00" + o.Password
			enc := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
			base64.StdEncoding.Encode(enc, []byte(raw))
			fmt.Fprintf(c.conn, "<auth xmlns='%s' mechanism='PLAIN'>%s</auth>\n",
				nsSASL, enc)
			break
		}
		if m == "DIGEST-MD5" {
			mechanism = m
			// Digest-MD5 authentication
			fmt.Fprintf(c.conn, "<auth xmlns='%s' mechanism='DIGEST-MD5'/>\n",
				nsSASL)
			var ch saslChallenge
			if err = c.p.DecodeElement(&ch, nil); err != nil {
				return errors.New("unmarshal <challenge>: " + err.Error())
			}
			b, err := base64.StdEncoding.DecodeString(string(ch))
			if err != nil {
				return err
			}
			tokens := map[string]string{}
			for _, token := range strings.Split(string(b), ",") {
				kv := strings.SplitN(strings.TrimSpace(token), "=", 2)
				if len(kv) == 2 {
					if kv[1][0] == '"' && kv[1][len(kv[1])-1] == '"' {
						kv[1] = kv[1][1 : len(kv[1])-1]
					}
					tokens[kv[0]] = kv[1]
				}
			}
			realm, _ := tokens["realm"]
			nonce, _ := tokens["nonce"]
			qop, _ := tokens["qop"]
			charset, _ := tokens["charset"]
			cnonceStr := cnonce()
			digestUri := "xmpp/" + domain
			nonceCount := fmt.Sprintf("%08x", 1)
			digest := saslDigestResponse(user, realm, o.Password, nonce, cnonceStr, "AUTHENTICATE", digestUri, nonceCount)
			message := "username=\"" + user + "\", realm=\"" + realm + "\", nonce=\"" + nonce + "\", cnonce=\"" + cnonceStr + "\", nc=" + nonceCount + ", qop=" + qop + ", digest-uri=\"" + digestUri + "\", response=" + digest + ", charset=" + charset

			fmt.Fprintf(c.conn, "<response xmlns='%s'>%s</response>\n", nsSASL, base64.StdEncoding.EncodeToString([]byte(message)))

			var rspauth saslRspAuth
			if err = c.p.DecodeElement(&rspauth, nil); err != nil {
				return errors.New("unmarshal <challenge>: " + err.Error())
			}
			b, err = base64.StdEncoding.DecodeString(string(rspauth))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.conn, "<response xmlns='%s'/>\n", nsSASL)
			break
		}
	}
	if mechanism == "" {
		return errors.New(fmt.Sprintf("PLAIN authentication is not an option: %v", f.Mechanisms.Mechanism))
	}

	// Next message should be either success or failure.
	name, val, err := next(c.p)
	if err != nil {
		return err
	}
	switch v := val.(type) {
	case *saslSuccess:
	case *saslFailure:
		// v.Any is type of sub-element in failure,
		// which gives a description of what failed.
		return errors.New("auth failure: " + v.Any.Local)
	default:
		return errors.New("expected <success> or <failure>, got <" + name.Local + "> in " + name.Space)
	}

	// Now that we're authenticated, we're supposed to start the stream over again.
	// Declare intent to be a jabber client.
	if f, err = c.startStream(o, domain); err != nil {
		return err
	}

	//Generate uniq cookie
	cookie := getCookie()

	// Send IQ message asking to bind to the local user name.
	if o.Resource == "" {
		fmt.Fprintf(c.conn, "<iq type='set' id='%x'><bind xmlns='%s'></bind></iq>\n", cookie, nsBind)
	} else {
		fmt.Fprintf(c.conn, "<iq type='set' id='%x'><bind xmlns='%s'><resource>%s</resource></bind></iq>\n", cookie, nsBind, o.Resource)
	}
	var iq clientIQ
	if err = c.p.DecodeElement(&iq, nil); err != nil {
		return errors.New("unmarshal <iq>: " + err.Error())
	}
	if &iq.Bind == nil {
		return errors.New("<iq> result missing <bind>")
	}
	c.jid = iq.Bind.Jid // our local id

	if o.Session {
		//if server support session, open it
		fmt.Fprintf(c.conn, "<iq to='%s' type='set' id='%x'><session xmlns='%s'/></iq>", xmlEscape(domain), cookie, NsSession)
	}

	// We're connected and can now receive and send messages.
	fmt.Fprintf(c.conn, "<presence xml:lang='en'><show>%s</show><status>%s</status></presence>", o.Status, o.StatusMessage)

	return nil
}

// startTlsIfRequired examines the server's stream features and, if STARTTLS is required or supported, performs the TLS handshake.
// f will be updated if the handshake completes, as the new stream's features are typically different from the original.
func (c *Client) startTlsIfRequired(f *streamFeatures, o *Options, domain string) (*streamFeatures, error) {
	// whether we start tls is a matter of opinion: the server's and the user's.
	switch {
	case f.StartTLS == nil:
		// the server does not support STARTTLS
		return f, nil
	case f.StartTLS.Required != nil:
		// the server requires STARTTLS.
	case !o.StartTLS:
		// the user wants STARTTLS and the server supports it.
	}
	var err error

	fmt.Fprintf(c.conn, "<starttls xmlns='urn:ietf:params:xml:ns:xmpp-tls'/>\n")
	var k tlsProceed
	if err = c.p.DecodeElement(&k, nil); err != nil {
		return f, errors.New("unmarshal <proceed>: "+err.Error())
	}

	tc := o.TLSConfig
	if tc == nil {
		tc = new(tls.Config)
		*tc = DefaultConfig
		//TODO(scott): we should consider using the server's address or reverse lookup
		tc.ServerName = domain
	}
	t := tls.Client(c.conn, tc)

	if err = t.Handshake(); err != nil {
		return f, errors.New("starttls handshake: "+err.Error())
	}
	c.conn = t

	// restart our declaration of XMPP stream intentions.
	tf, err := c.startStream(o, domain)
	if err != nil {
		return f, err
	}
	return tf, nil
}

// startStream will start a new XML decoder for the connection, signal the start of a stream to the server and verify that the server has
// also started the stream; if o.Debug is true, startStream will tee decoded XML data to stdout.  The features advertised by the server
// will be returned.
func (c *Client) startStream(o *Options, domain string) (*streamFeatures, error) {
	c.p = xml.NewDecoder(c.conn)
	if o.Debug {
		c.p = xml.NewDecoder(tee{c.conn, os.Stdout})
	}

	_, err := fmt.Fprintf(c.conn, "<?xml version='1.0'?>\n" +
				"<stream:stream to='%s' xmlns='%s'\n" +
				" xmlns:stream='%s' version='1.0'>\n",
		xmlEscape(domain), nsClient, nsStream)
	if err != nil {
		return nil, err
	}

	// We expect the server to start a <stream>.
	se, err := nextStart(c.p)
	if err != nil {
		return nil, err
	}
	if se.Name.Space != nsStream || se.Name.Local != "stream" {
		return nil, fmt.Errorf("expected <stream> but got <%v> in %v", se.Name.Local, se.Name.Space)
	}

	// Now we're in the stream and can use Unmarshal.
	// Next message should be <features> to tell us authentication options.
	// See section 4.6 in RFC 3920.
	f := new(streamFeatures)
	if err = c.p.DecodeElement(f, nil); err != nil {
		return f, errors.New("unmarshal <features>: "+err.Error())
	}
	return f, nil
}

// IsEncrypted will return true if the client is connected using a TLS transport, either because it used
// TLS to connect from the outset, or because it successfully used STARTTLS to promote a TCP connection
// to TLS.
func (c *Client) IsEncrypted() bool {
	_, ok := c.conn.(*tls.Conn)
	return ok
}

type Chat struct {
	Remote string
	Type   string
	Text   string
	Other  []string
}

type Presence struct {
	From string
	To   string
	Type string
	Show string
}

// Recv wait next token of chat.
func (c *Client) Recv() (event interface{}, err error) {
	for {
		_, val, err := next(c.p)
		if err != nil {
			return Chat{}, err
		}
		switch v := val.(type) {
		case *clientMessage:
			return Chat{v.From, v.Type, v.Body, v.Other}, nil
		case *clientPresence:
			return Presence{v.From, v.To, v.Type, v.Show}, nil
		}
	}
	panic("unreachable")
}

// Send sends message text.
func (c *Client) Send(chat Chat) {
	_, err := fmt.Fprintf(c.conn, "<message to='%s' type='%s' xml:lang='en'>" +
				"<body>%s</body></message>",
		xmlEscape(chat.Remote), xmlEscape(chat.Type), xmlEscape(chat.Text))
		
	if err != nil {
		log.Fatal(err)
	}	
}

// Send origin
func (c *Client) SendOrg(org string) {
	fmt.Fprint(c.conn, org)
}

// RFC 3920  C.1  Streams name space
type streamFeatures struct {
	XMLName    xml.Name `xml:"http://etherx.jabber.org/streams features"`
	StartTLS   *tlsStartTLS
	Mechanisms saslMechanisms
	Bind       bindBind
	Session    bool
}

type streamError struct {
	XMLName xml.Name `xml:"http://etherx.jabber.org/streams error"`
	Any     xml.Name
	Text    string
}

// RFC 3920  C.3  TLS name space

type tlsStartTLS struct {
	XMLName  xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-tls starttls"`
	Required *string  `xml:"required"`
}

type tlsProceed struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-tls proceed"`
}

type tlsFailure struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-tls failure"`
}

// RFC 3920  C.4  SASL name space

type saslMechanisms struct {
	XMLName   xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl mechanisms"`
	Mechanism []string `xml:"mechanism"`
}

type saslAuth struct {
	XMLName   xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl auth"`
	Mechanism string   `xml:",attr"`
}

type saslChallenge string

type saslRspAuth string

type saslResponse string

type saslAbort struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl abort"`
}

type saslSuccess struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl success"`
}

type saslFailure struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl failure"`
	Any     xml.Name
}

// RFC 3920  C.5  Resource binding name space

type bindBind struct {
	XMLName  xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
	Resource string
	Jid      string `xml:"jid"`
}

// RFC 3921  B.1  jabber:client

type clientMessage struct {
	XMLName xml.Name `xml:"jabber:client message"`
	From    string   `xml:"from,attr"`
	Id      string   `xml:"id,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr"` // chat, error, groupchat, headline, or normal

	// These should technically be []clientText,
	// but string is much more convenient.
	Subject string `xml:"subject"`
	Body    string `xml:"body"`
	Thread  string `xml:"thread"`

	// Any hasn't matched element
	Other []string `xml:",any"`
}

type clientText struct {
	Lang string `xml:",attr"`
	Body string `xml:"chardata"`
}

type clientPresence struct {
	XMLName xml.Name `xml:"jabber:client presence"`
	From    string   `xml:"from,attr"`
	Id      string   `xml:"id,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr"` // error, probe, subscribe, subscribed, unavailable, unsubscribe, unsubscribed
	Lang    string   `xml:"lang,attr"`

	Show     string `xml:"show"`        // away, chat, dnd, xa
	Status   string `xml:"status,attr"` // sb []clientText
	Priority string `xml:"priority,attr"`
	Error    *clientError
}

type clientIQ struct { // info/query
	XMLName xml.Name `xml:"jabber:client iq"`
	From    string   `xml:",attr"`
	Id      string   `xml:",attr"`
	To      string   `xml:",attr"`
	Type    string   `xml:",attr"` // error, get, result, set
	Error   clientError
	Bind    bindBind
}

type clientError struct {
	XMLName xml.Name `xml:"jabber:client error"`
	Code    string   `xml:",attr"`
	Type    string   `xml:",attr"`
	Any     xml.Name
	Text    string
}

// Scan XML token stream to find next StartElement.
func nextStart(p *xml.Decoder) (xml.StartElement, error) {
	for {
		t, err := p.Token()
		if err != nil && err != io.EOF {
			return xml.StartElement{}, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			return t, nil
		}
	}
	panic("unreachable")
}

// Scan XML token stream for next element and save into val.
// If val == nil, allocate new element based on proto map.
// Either way, return val.
func next(p *xml.Decoder) (xml.Name, interface{}, error) {
	// Read start element to find out what type we want.
	se, err := nextStart(p)
	if err != nil {
		return xml.Name{}, nil, err
	}

	// Put it in an interface and allocate one.
	var nv interface{}
	switch se.Name.Space + " " + se.Name.Local {
	case nsStream + " features":
		nv = &streamFeatures{}
	case nsStream + " error":
		nv = &streamError{}
	case nsTLS + " starttls":
		nv = &tlsStartTLS{}
	case nsTLS + " proceed":
		nv = &tlsProceed{}
	case nsTLS + " failure":
		nv = &tlsFailure{}
	case nsSASL + " mechanisms":
		nv = &saslMechanisms{}
	case nsSASL + " challenge":
		nv = ""
	case nsSASL + " response":
		nv = ""
	case nsSASL + " abort":
		nv = &saslAbort{}
	case nsSASL + " success":
		nv = &saslSuccess{}
	case nsSASL + " failure":
		nv = &saslFailure{}
	case nsBind + " bind":
		nv = &bindBind{}
	case nsClient + " message":
		nv = &clientMessage{}
	case nsClient + " presence":
		nv = &clientPresence{}
	case nsClient + " iq":
		nv = &clientIQ{}
	case nsClient + " error":
		nv = &clientError{}
	default:
		return xml.Name{}, nil, errors.New("unexpected XMPP message " +
				se.Name.Space+" <"+se.Name.Local+"/>")
	}

	// Unmarshal into that storage.
	if err = p.DecodeElement(nv, &se); err != nil {
		return xml.Name{}, nil, err
	}
	return se.Name, nv, err
}

var xmlSpecial = map[byte]string{
	'<':  "&lt;",
	'>':  "&gt;",
	'"':  "&quot;",
	'\'': "&apos;",
	'&':  "&amp;",
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if s, ok := xmlSpecial[c]; ok {
			b.WriteString(s)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

type tee struct {
	r io.Reader
	w io.Writer
}

func (t tee) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	if n > 0 {
		t.w.Write(p[0:n])
		t.w.Write([]byte("\n"))
	}
	return
}
