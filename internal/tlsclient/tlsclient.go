// Package tlsclient wraps bogdanfinn/tls-client and returns standard
// *http.Response, keeping fhttp internal.  Modeled on aurora-develop/aurora.
package tlsclient

import (
	"io"
	"net/http"
	"net/url"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/net/proxy"
)

// Client wraps a tls_client.HttpClient and exposes a standard-http-like API.
type Client struct {
	inner     tls_client.HttpClient
	ReqBefore func(r *fhttp.Request) error
}

// Option is a builder for New().
type Option func(*options)

type options struct {
	proxyURL string
}

// WithProxy sets the proxy URL.
func WithProxy(raw string) Option {
	return func(o *options) { o.proxyURL = raw }
}

// New creates a *Client with Chrome_146 TLS fingerprinting.
func New(opts ...Option) *Client {
	o := &options{}
	for _, fn := range opts {
		fn(o)
	}
	baseOpts := []tls_client.HttpClientOption{
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithTimeoutSeconds(120),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithForceHttp1(),
		tls_client.WithNotFollowRedirects(),
	}
	if o.proxyURL != "" {
		baseOpts = append(baseOpts, tls_client.WithProxyUrl(o.proxyURL))
	}
	inner, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), baseOpts...)
	if err != nil {
		panic("tlsclient.New: " + err.Error())
	}
	return &Client{inner: inner}
}

// Do sends a standard *http.Request and returns a standard *http.Response.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	freq, err := toFhttp(req)
	if err != nil {
		return nil, err
	}
	if c.ReqBefore != nil {
		if err := c.ReqBefore(freq); err != nil {
			return nil, err
		}
	}
	fresp, err := c.inner.Do(freq)
	if err != nil {
		return nil, err
	}
	return toNetHTTP(fresp), nil
}

// SetProxy changes the proxy at runtime.
func (c *Client) SetProxy(raw string) error { return c.inner.SetProxy(raw) }

// GetDialer returns the browser-profiled network dialer used by the wrapped client.
func (c *Client) GetDialer() proxy.ContextDialer { return c.inner.GetDialer() }

// GetTLSDialer returns the browser-profiled TLS dialer used by the wrapped client.
func (c *Client) GetTLSDialer() tls_client.TLSDialerFunc { return c.inner.GetTLSDialer() }

// CloseIdleConnections closes idle connections held by the wrapped client.
func (c *Client) CloseIdleConnections() { c.inner.CloseIdleConnections() }

// GetCookies returns cookies from the jar (as net/http.Cookie).
func (c *Client) GetCookies(u *url.URL) []*http.Cookie {
	jar := c.inner.GetCookieJar()
	if jar == nil {
		return nil
	}
	fcs := jar.Cookies(u)
	if len(fcs) == 0 {
		return nil
	}
	cs := make([]*http.Cookie, len(fcs))
	for i, fc := range fcs {
		cs[i] = cookieToNet(fc)
	}
	return cs
}

// SetCookies stores net/http.Cookies into the jar.
func (c *Client) SetCookies(u *url.URL, cookies []*http.Cookie) {
	jar := c.inner.GetCookieJar()
	if jar == nil {
		return
	}
	fcs := make([]*fhttp.Cookie, len(cookies))
	for i, ck := range cookies {
		fcs[i] = &fhttp.Cookie{
			Name:     ck.Name,
			Value:    ck.Value,
			Path:     ck.Path,
			Domain:   ck.Domain,
			Expires:  ck.Expires,
			Secure:   ck.Secure,
			HttpOnly: ck.HttpOnly,
			SameSite: fhttp.SameSite(ck.SameSite),
		}
	}
	jar.SetCookies(u, fcs)
}

// ---- conversions ---------------------------------------------------------

func toFhttp(req *http.Request) (*fhttp.Request, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}
	freq, err := fhttp.NewRequest(method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			freq.Header.Set(k, v)
		}
	}
	for _, ck := range req.Cookies() {
		freq.AddCookie(&fhttp.Cookie{
			Name:     ck.Name,
			Value:    ck.Value,
			Path:     ck.Path,
			Domain:   ck.Domain,
			Expires:  ck.Expires,
			Secure:   ck.Secure,
			HttpOnly: ck.HttpOnly,
			SameSite: fhttp.SameSite(ck.SameSite),
		})
	}
	return freq, nil
}

func toNetHTTP(fresp *fhttp.Response) *http.Response {
	return &http.Response{
		Status:           fresp.Status,
		StatusCode:       fresp.StatusCode,
		Proto:            fresp.Proto,
		ProtoMajor:       fresp.ProtoMajor,
		ProtoMinor:       fresp.ProtoMinor,
		Header:           http.Header(fresp.Header),
		Body:             fresp.Body,
		ContentLength:    fresp.ContentLength,
		TransferEncoding: fresp.TransferEncoding,
		Close:            fresp.Close,
		Uncompressed:     fresp.Uncompressed,
		Trailer:          http.Header(fresp.Trailer),
	}
}

func cookieToNet(fc *fhttp.Cookie) *http.Cookie {
	return &http.Cookie{
		Name:     fc.Name,
		Value:    fc.Value,
		Path:     fc.Path,
		Domain:   fc.Domain,
		Expires:  fc.Expires,
		Secure:   fc.Secure,
		HttpOnly: fc.HttpOnly,
		SameSite: http.SameSite(fc.SameSite),
	}
}
