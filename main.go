package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"golang.org/x/net/http2"
)

type Report struct {
	Address string
	Header  http.Header
	Proto   string
	Status  string
	Timing  Timing
}

type Timing struct {
	DNS      int
	TCP      int
	TLS      int
	Server   int
	Transfer int

	Lookup        int
	Connect       int
	PreTransfer   int
	StartTransfer int
	Total         int
}

const (
	httpsTemplate = `` +
		`  DNS Lookup   TCP Connection   TLS Handshake   Server Processing   Content Transfer` + "\n" +
		`[    %>DNS  |         %>TCP  |        %>TLS  |         %>Server  |      %>Transfer  ]` + "\n" +
		`            |                |               |                   |                  |` + "\n" +
		`   namelookup:%<Lookup       |               |                   |                  |` + "\n" +
		`                       connect:%<Connect     |                   |                  |` + "\n" +
		`                                   pretransfer:%<PreTransfer     |                  |` + "\n" +
		`                                                     starttransfer:%<StartTransfer  |` + "\n" +
		`                                                                                total:%<Total` + "\n"

	httpTemplate = `` +
		`  DNS Lookup   TCP Connection   Server Processing   Content Transfer` + "\n" +
		`[    %>DNS  |         %>TCP  |         %>Server  |      %>Transfer  ]` + "\n" +
		`            |                |                   |                  |` + "\n" +
		`   namelookup:%<Lookup       |                   |                  |` + "\n" +
		`                       connect:%<Connect         |                  |` + "\n" +
		`                                     starttransfer:%<StartTransfer  |` + "\n" +
		`                                                                total:%<Total` + "\n"
)

var (
	// Command line flags.
	httpMethod      string
	postBody        string
	followRedirects bool
	onlyHeader      bool
	insecure        bool
	httpHeaders     headers
	saveOutput      bool
	outputFile      string
	showVersion     bool
	clientCertFile  string
	fourOnly        bool
	sixOnly         bool
	maxTime         time.Duration
	cacert          string
	jsonOutput      bool
	numRequests     int
	requestDelay    time.Duration

	// number of redirects followed
	redirectsFollowed int

	version = "devel" // for -v flag, updated during the release process with -ldflags=-X=main.version=...
)

const maxRedirects = 10

func init() {
	flag.StringVar(&httpMethod, "X", "GET", "HTTP method to use")
	flag.StringVar(&postBody, "d", "", "the body of a POST or PUT request; from file use @filename")
	flag.BoolVar(&followRedirects, "L", false, "follow 30x redirects")
	flag.BoolVar(&onlyHeader, "I", false, "don't read body of request")
	flag.BoolVar(&insecure, "k", false, "allow insecure SSL connections")
	flag.Var(&httpHeaders, "H", "set HTTP header; repeatable: -H 'Accept: ...' -H 'Range: ...'")
	flag.BoolVar(&saveOutput, "O", false, "save body as remote filename")
	flag.StringVar(&outputFile, "o", "", "output file for body")
	flag.BoolVar(&showVersion, "v", false, "print version number")
	flag.StringVar(&clientCertFile, "E", "", "client cert file for tls config")
	flag.BoolVar(&fourOnly, "4", false, "resolve IPv4 addresses only")
	flag.BoolVar(&sixOnly, "6", false, "resolve IPv6 addresses only")
	flag.DurationVar(&maxTime, "m", 0, "maximum time allowed for the transfer")
	flag.StringVar(&cacert, "cacert", "", "CA certificate to verify peer against (SSL)")
	flag.BoolVar(&jsonOutput, "J", false, "use JSON to output results")
	flag.IntVar(&numRequests, "n", 1, "number of requests")
	flag.DurationVar(&requestDelay, "w", 3*time.Second, "delay between requests")

	flag.Usage = usage
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] URL\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "OPTIONS:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "ENVIRONMENT:")
	fmt.Fprintln(os.Stderr, "  HTTP_PROXY    proxy for HTTP requests; complete URL or HOST[:PORT]")
	fmt.Fprintln(os.Stderr, "                used for HTTPS requests if HTTPS_PROXY undefined")
	fmt.Fprintln(os.Stderr, "  HTTPS_PROXY   proxy for HTTPS requests; complete URL or HOST[:PORT]")
	fmt.Fprintln(os.Stderr, "  NO_PROXY      comma-separated list of hosts to exclude from proxy")
}

func printf(format string, a ...interface{}) (n int, err error) {
	return fmt.Fprintf(color.Output, format, a...)
}

func grayscale(code color.Attribute) func(string, ...interface{}) string {
	return color.New(code + 232).SprintfFunc()
}

func main() {
	flag.Parse()

	if showVersion {
		fmt.Printf("%s %s (runtime: %s)\n", os.Args[0], version, runtime.Version())
		os.Exit(0)
	}

	if fourOnly && sixOnly {
		fmt.Fprintf(os.Stderr, "%s: Only one of -4 and -6 may be specified\n", os.Args[0])
		os.Exit(-1)
	}

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		os.Exit(2)
	}

	if (httpMethod == "POST" || httpMethod == "PUT") && postBody == "" {
		log.Fatal("must supply post body using -d when POST or PUT is used")
	}

	if onlyHeader {
		httpMethod = "HEAD"
	}

	url := parseURL(args[0])

	visit(url)
}

// readCACerts - helper function to load additional CA certificates
func readCACerts(filename string) (*x509.CertPool, error) {
	if filename == "" {
		return nil, nil
	}
	certFileBytes, err := ioutil.ReadFile(filename)

	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate file: %v", err)
	}

	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(certFileBytes)

	return certPool, nil
}

// readClientCert - helper function to read client certificate
// from pem formatted file
func readClientCert(filename string) ([]tls.Certificate, error) {
	if filename == "" {
		return nil, nil
	}
	var (
		pkeyPem []byte
		certPem bytes.Buffer
	)

	// read client certificate file (must include client private key and certificate)
	certFileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read client certificate file: %v", err)
	}

	for {
		block, rest := pem.Decode(certFileBytes)
		if block == nil {
			break
		}
		certFileBytes = rest

		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			pkeyPem = pem.EncodeToMemory(block)
		}
		if strings.HasSuffix(block.Type, "CERTIFICATE") {
			err = pem.Encode(&certPem, block)
			if err != nil {
				return nil, fmt.Errorf("failed to read client certificate file: %v", err)
			}
		}
	}

	cert, err := tls.X509KeyPair(certPem.Bytes(), pkeyPem)
	if err != nil {
		return nil, fmt.Errorf("unable to load client cert and key pair: %v", err)
	}

	return []tls.Certificate{cert}, nil
}

func parseURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, err := url.Parse(uri)
	if err != nil {
		log.Fatalf("could not parse url %q: %v", uri, err)
	}

	if url.Scheme == "" {
		url.Scheme = "http"
		if !strings.HasSuffix(url.Host, ":80") {
			url.Scheme += "s"
		}
	}
	return url
}

func headerKeyValue(h string) (string, string) {
	i := strings.Index(h, ":")
	if i == -1 {
		log.Fatalf("Header '%s' has invalid format, missing ':'", h)
	}
	return strings.TrimRight(h[:i], " "), strings.TrimLeft(h[i:], " :")
}

func dialContext(network string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: false,
		}).DialContext(ctx, network, addr)
	}
}

// visit visits a url and times the interaction.
// If the response is a 30x, visit follows the redirect.
func visit(url *url.URL) {
	req := newRequest(httpMethod, url, postBody)

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	switch {
	case fourOnly:
		tr.DialContext = dialContext("tcp4")
	case sixOnly:
		tr.DialContext = dialContext("tcp6")
	}

	switch url.Scheme {
	case "https":
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = req.Host
		}

		cert, err := readClientCert(clientCertFile)
		if err != nil {
			log.Fatal(err)
		}
		rootCAs, err := readCACerts(cacert)
		if err != nil {
			log.Printf("warning: failed to read CA certificates: %s\n", err)
		}

		tr.TLSClientConfig = &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecure,
			Certificates:       cert,
			RootCAs:            rootCAs,
		}

		// Because we create a custom TLSClientConfig, we have to opt-in to HTTP/2.
		// See https://github.com/golang/go/issues/14275
		err = http2.ConfigureTransport(tr)
		if err != nil {
			log.Fatalf("failed to prepare transport for HTTP/2: %v", err)
		}
	}

	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// always refuse to follow redirects, visit does that
			// manually if required.
			return http.ErrUseLastResponse
		},
		Timeout: maxTime,
	}

	for i := 0; i < numRequests; i++ {
		if i > 0 {
			time.Sleep(requestDelay)
		}

		var tStart, tDNSStart, tConnectStart, tTLSStart, tConnected, tTTFB time.Time
		var report Report

		trace := &httptrace.ClientTrace{
			GetConn:  func(_ string) { tStart = time.Now() },
			DNSStart: func(_ httptrace.DNSStartInfo) { tDNSStart = time.Now() },
			DNSDone: func(_ httptrace.DNSDoneInfo) {
				report.Timing.DNS = msSince(tDNSStart)
				report.Timing.Lookup = msSince(tStart)
			},
			ConnectStart: func(_, _ string) {
				if tConnectStart.IsZero() {
					// connecting to IP
					tConnectStart = time.Now()
				}
			},
			ConnectDone: func(net, addr string, err error) {
				if err != nil {
					log.Fatalf("unable to connect to host %v: %v", addr, err)
				}
				report.Timing.TCP = msSince(tConnectStart)
				report.Timing.Connect = msSince(tStart)

				report.Address = addr
				if !jsonOutput {
					printf("\n%s%s\n", color.GreenString("Connected to "), color.CyanString(addr))
				}
			},
			TLSHandshakeStart: func() { tTLSStart = time.Now() },
			TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
				report.Timing.TLS = msSince(tTLSStart)
			},
			GotConn: func(_ httptrace.GotConnInfo) {
				tConnected = time.Now()
				report.Timing.PreTransfer = msSince(tStart)
			},
			GotFirstResponseByte: func() {
				tTTFB = time.Now()
				report.Timing.Server = msSince(tConnected)
				report.Timing.StartTransfer = msSince(tStart)
			},
		}
		req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))

		resp, err := client.Do(req)
		if err != nil {
			log.Fatalf("failed to read response: %v", err)
		}

		bodyMsg := readResponseBody(req, resp)
		resp.Body.Close()

		// after read body
		report.Timing.Transfer = msSince(tTTFB)
		report.Timing.Total = msSince(tStart)

		report.Proto = resp.Proto
		report.Status = resp.Status
		report.Header = resp.Header

		// print status line and headers
		if jsonOutput {
			b, err := json.Marshal(report)
			if err != nil {
				log.Fatalf("unable to marshal json report: %v", err)
			}
			fmt.Printf("%s\n", b)
		} else {
			printf("\n%s%s%s\n", color.GreenString("HTTP"), grayscale(14)("/"), color.CyanString("%d.%d %s", resp.ProtoMajor, resp.ProtoMinor, resp.Status))

			names := make([]string, 0, len(resp.Header))
			for k := range resp.Header {
				names = append(names, k)
			}
			sort.Sort(headers(names))
			for _, k := range names {
				printf("%s %s\n", grayscale(14)(k+":"), color.CyanString(strings.Join(resp.Header[k], ",")))
			}

			if bodyMsg != "" {
				printf("\n%s\n", bodyMsg)
			}

			fmt.Println()

			switch url.Scheme {
			case "https":
				printTemplate(httpsTemplate, report.Timing)
			case "http":
				printTemplate(httpTemplate, report.Timing)
			}
		}

		if followRedirects && isRedirect(resp) {
			loc, err := resp.Location()
			if err != nil {
				if err == http.ErrNoLocation {
					// 30x but no Location to follow, give up.
					return
				}
				log.Fatalf("unable to follow redirect: %v", err)
			}

			redirectsFollowed++
			if redirectsFollowed > maxRedirects {
				log.Fatalf("maximum number of redirects (%d) followed", maxRedirects)
			}

			visit(loc)
		}
	}
}

func msSince(t time.Time) int {
	return int(time.Now().Sub(t) / time.Millisecond)
}

func printTemplate(tmpl string, vars Timing) {
	rvars := reflect.ValueOf(vars)
	b := []byte(tmpl)
	for idx := bytes.IndexByte(b, '%'); idx != -1; idx = bytes.IndexByte(b, '%') {
		dir := b[idx+1]
		end := idx + 2
		for ; end < len(b) && ((b[end] >= 'a' && b[end] <= 'z') || (b[end] >= 'A' && b[end] <= 'Z')); end++ {
		}
		vnam := string(b[idx+2 : end])
		copy(b[idx:end], bytes.Repeat([]byte{' '}, end-idx))
		val := rvars.FieldByName(vnam)
		if !val.IsValid() {
			panic("invalid template variable: " + vnam)
		}
		v := strconv.Itoa(val.Interface().(int)) + "ms"
		vlen := len(v)
		v = color.CyanString(v)
		switch dir {
		case '>':
			b = append(append(append([]byte{}, b[:end-vlen]...), []byte(v)...), b[end:]...)
		case '<':
			b = append(append(append([]byte{}, b[:idx]...), []byte(v)...), b[idx+vlen:]...)
		default:
			panic("invalid direction: " + string(dir))
		}
		idx = end
	}
	print(string(b))
}

func isRedirect(resp *http.Response) bool {
	return resp.StatusCode > 299 && resp.StatusCode < 400
}

func newRequest(method string, url *url.URL, body string) *http.Request {
	req, err := http.NewRequest(method, url.String(), createBody(body))
	if err != nil {
		log.Fatalf("unable to create request: %v", err)
	}
	for _, h := range httpHeaders {
		k, v := headerKeyValue(h)
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Add(k, v)
	}
	return req
}

func createBody(body string) io.Reader {
	if strings.HasPrefix(body, "@") {
		filename := body[1:]
		f, err := os.Open(filename)
		if err != nil {
			log.Fatalf("failed to open data file %s: %v", filename, err)
		}
		return f
	}
	return strings.NewReader(body)
}

// getFilenameFromHeaders tries to automatically determine the output filename,
// when saving to disk, based on the Content-Disposition header.
// If the header is not present, or it does not contain enough information to
// determine which filename to use, this function returns "".
func getFilenameFromHeaders(headers http.Header) string {
	// if the Content-Disposition header is set parse it
	if hdr := headers.Get("Content-Disposition"); hdr != "" {
		// pull the media type, and subsequent params, from
		// the body of the header field
		mt, params, err := mime.ParseMediaType(hdr)

		// if there was no error and the media type is attachment
		if err == nil && mt == "attachment" {
			if filename := params["filename"]; filename != "" {
				return filename
			}
		}
	}

	// return an empty string if we were unable to determine the filename
	return ""
}

// readResponseBody consumes the body of the response.
// readResponseBody returns an informational message about the
// disposition of the response body's contents.
func readResponseBody(req *http.Request, resp *http.Response) string {
	if isRedirect(resp) || req.Method == http.MethodHead {
		return ""
	}

	w := ioutil.Discard
	msg := color.CyanString("Body discarded")

	if saveOutput || outputFile != "" {
		filename := outputFile

		if saveOutput {
			// try to get the filename from the Content-Disposition header
			// otherwise fall back to the RequestURI
			if filename = getFilenameFromHeaders(resp.Header); filename == "" {
				filename = path.Base(req.URL.RequestURI())
			}

			if filename == "/" {
				log.Fatalf("No remote filename; specify output filename with -o to save response body")
			}
		}

		f, err := os.Create(filename)
		if err != nil {
			log.Fatalf("unable to create file %s: %v", filename, err)
		}
		defer f.Close()
		w = f
		msg = color.CyanString("Body read")
	}

	if _, err := io.Copy(w, resp.Body); err != nil && w != ioutil.Discard {
		log.Fatalf("failed to read response body: %v", err)
	}

	return msg
}

type headers []string

func (h headers) String() string {
	var o []string
	for _, v := range h {
		o = append(o, "-H "+v)
	}
	return strings.Join(o, " ")
}

func (h *headers) Set(v string) error {
	*h = append(*h, v)
	return nil
}

func (h headers) Len() int      { return len(h) }
func (h headers) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h headers) Less(i, j int) bool {
	a, b := h[i], h[j]

	// server always sorts at the top
	if a == "Server" {
		return true
	}
	if b == "Server" {
		return false
	}

	endtoend := func(n string) bool {
		// https://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html#sec13.5.1
		switch n {
		case "Connection",
			"Keep-Alive",
			"Proxy-Authenticate",
			"Proxy-Authorization",
			"TE",
			"Trailers",
			"Transfer-Encoding",
			"Upgrade":
			return false
		default:
			return true
		}
	}

	x, y := endtoend(a), endtoend(b)
	if x == y {
		// both are of the same class
		return a < b
	}
	return x
}
