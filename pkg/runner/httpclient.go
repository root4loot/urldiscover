package runner

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"

	"github.com/root4loot/goutils/httputil"
	"github.com/root4loot/urlwalk/pkg/log"
	"github.com/root4loot/urlwalk/pkg/options"
	"github.com/root4loot/urlwalk/pkg/util"
)

type HTTPClient struct {
	client *http.Client
}

// NewHTTPClient returns a new HTTP client
func NewHTTPClient(options *options.Options) *HTTPClient {
	var client *http.Client

	// new client with optional resolvers
	if len(options.Resolvers) > 0 {
		client, _ = httputil.ClientWithOptionalResolvers(options.Resolvers...)
	} else {
		client, _ = httputil.ClientWithOptionalResolvers()
	}

	// set client transport
	client.Transport = &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConnsPerHost:   options.Concurrency,
		ResponseHeaderTimeout: time.Duration(options.Timeout) * time.Second,
	}
	client.Timeout = time.Duration(options.Timeout) * time.Second

	if options.Proxy != "" {
		if !util.HasScheme(options.Proxy) {
			options.Proxy = "http://" + options.Proxy
		}
		proxy, err := url.Parse(options.Proxy)
		if err != nil {
			log.Fatalf("Error parsing proxy URL: %s", err)
		}

		client = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyURL(proxy),
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
				MaxIdleConnsPerHost:   options.Concurrency,
				ResponseHeaderTimeout: time.Duration(options.Timeout) * time.Second,
			},
			Timeout: time.Duration(options.Timeout) * time.Second,
		}
	}

	return &HTTPClient{client: client}
}
