// Author: Daniel Antonsen (@danielantonsen)
// Distributed Under MIT License

package runner

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/purell"
	"github.com/glaslos/ssdeep"
	"github.com/root4loot/goutils/domainutil"
	"github.com/root4loot/goutils/httputil"
	"github.com/root4loot/goutils/log"
	"github.com/root4loot/goutils/sliceutil"
	"github.com/root4loot/goutils/strutil"
	"github.com/root4loot/goutils/urlutil"
	"github.com/root4loot/recrawl/pkg/options"
	"github.com/root4loot/scope"
)

var (
	re_path     = regexp.MustCompile(`(?:"|')(?:(((?:[a-zA-Z]{1,10}:(?:\\)?/(?:\\)?/|//)[^"'/]{1,}\.[a-zA-Z]{2,}[^"']*)|((?:/|\.\./|\./|\\/)[^"'><,;|*()(%%$^/\\\[\]][^"'><,;|()]*[^"'><,;|()]*))|([a-zA-Z0-9_\-/]{1,}/[a-zA-Z0-9_\-/]*\.[a-zA-Z0-9_]+(?:[\?|#][^"|']*)?)|([a-zA-Z0-9_\-/]{1,}/[a-zA-Z0-9_\-/]{3,}(?:[\?|#][^"|']*)?)|([a-zA-Z0-9_\-]+(?:\.[a-zA-Z0-9_]{1,})+)|([a-zA-Z0-9_\-/]+/))(?:"|')`)
	re_robots   = regexp.MustCompile(`(?:Allow|Disallow): \s*(.*)`)
	fuzzyHashes = make(map[string]map[string]bool) // Map of host to map of hash to bool
)

type Runner struct {
	Options *options.Options
	Results chan Result
	Scope   *scope.Scope
	client  *http.Client
}

type Result struct {
	RequestURL string
	StatusCode int
	Error      error
}

type Results struct {
	Results []Result
}

var (
	visitedURL  sync.Map
	visitedHost sync.Map
)

func init() {
	log.Init("recrawl")
}

func NewRunnerWithDefaults() *Runner {
	return newRunner(options.Default())
}

func NewRunnerWithOptions(o *options.Options) *Runner {
	return newRunner(o)
}

func newRunner(o *options.Options) *Runner {
	runner := &Runner{
		Results: make(chan Result),
		Options: o,
	}

	runner.setLogLevel()
	runner.initializeScope()
	runner.client = NewHTTPClient(o).client

	return runner
}

func (r *Runner) Run(targets ...string) {
	log.Debug("Run() called!")

	r.Options.ValidateOptions()
	r.Options.SetDefaultsMissing()
	c_queue, c_urls, c_wait := r.InitializeWorkerPool()

	log.Debug("number of targets: ", len(targets))

	for _, target := range targets {
		mainTarget, err := r.initializeTargetProcessing(target)

		if err != nil {
			log.Warn("Error preparing target:", err)
			continue
		}

		go r.queueURL(c_queue, mainTarget)
	}

	if len(targets) == 1 {
		r.Options.Concurrency = 1
	}

	log.Debug("starting workers")
	r.startWorkers(c_urls, c_queue, c_wait)
}

func (r *Runner) InitializeWorkerPool() (chan<- *url.URL, <-chan *url.URL, chan<- int) {
	c_wait := make(chan int)
	c_urls := make(chan *url.URL)
	c_queue := make(chan *url.URL)
	queueCount := 0

	timeoutDuration := time.Second * 7

	go func() {
		for delta := range c_wait {
			queueCount += delta
			if queueCount == 0 {
				close(c_queue)
				close(c_wait)
			}
		}
	}()

	go func() {
		timeoutTimer := time.NewTimer(timeoutDuration)
		defer timeoutTimer.Stop()

		for {
			select {
			case q := <-c_queue:
				if q != nil {
					if r.Scope.IsInScope(q.Host) && !r.isVisitedURL(q.String()) {
						c_urls <- q
					}

					if !timeoutTimer.Stop() {
						<-timeoutTimer.C
					}
					timeoutTimer.Reset(timeoutDuration)
				}
			case <-c_urls:

			case <-timeoutTimer.C:
				log.Debug("Timeout reached, closing channels.")
				close(c_urls)
				return
			}
		}
	}()

	return c_queue, c_urls, c_wait
}

func (r *Runner) initializeTargetProcessing(target string) (*url.URL, error) {
	if !strings.Contains(target, "://") {
		scheme, _, err := httputil.FindScheme(target)
		if err != nil {
			return nil, err
		}
		target = scheme + "://" + target
	}

	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		target = u.Scheme + "://" + u.Hostname()
	}

	if u.Host != "" {
		r.Scope.AddInclude(u.Host)
	}

	if !r.isVisitedHost(u.Hostname()) {
		r.addVisitedHost(u.Hostname())
	}

	u, err = url.Parse(target)
	if err != nil {
		return nil, err
	}

	return u, nil
}

func (r *Runner) initializeScope() {
	if r.Scope == nil {
		r.Scope = scope.NewScope()
	}

	for _, include := range r.Options.Include {
		r.Scope.AddInclude(include)
	}

	for _, exclude := range r.Options.Exclude {
		r.Scope.AddExclude(exclude)
	}
}

func (r *Runner) startWorkers(c_urls <-chan *url.URL, c_queue chan<- *url.URL, c_wait chan<- int) {
	var wg sync.WaitGroup
	for i := 0; i < r.Options.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Worker(c_urls, c_queue, c_wait, r.Results)
		}()
	}
	wg.Wait()
}

func (r *Runner) queueURL(c_queue chan<- *url.URL, url *url.URL) {
	url, err := url.Parse(r.cleanURL(url.String()))
	if err == nil {
		c_queue <- url
	}
}

func (r *Runner) Worker(c_urls <-chan *url.URL, c_queue chan<- *url.URL, c_wait chan<- int, c_result chan<- Result) {
	for c_url := range c_urls {
		log.Debugf("Processing URL: %s", c_url.String())

		if c_url == nil || c_url.Host == "" || r.isTrapped(c_url.Path) || r.isRedundantURL(c_url.String()) {
			log.Debugf("Skipping URL due to initial checks: %s", c_url)
			continue
		}

		if r.shouldAddRobotsTxt(c_url) {
			r.addRobotsTxtToQueue(c_url, c_queue, c_wait)
		}

		currentURL := c_url
		redirectCount := 0
		for {
			if redirectCount >= 10 {
				log.Infof("Redirect limit reached for %s", currentURL)
				break
			}

			_, resp, err := r.request(currentURL)
			if err != nil {
				log.Infof("Error requesting %s: %v", currentURL, err)
				r.Results <- Result{RequestURL: currentURL.String(), StatusCode: 0, Error: err}
				break
			}
			if resp == nil {
				break
			}

			if r.isRedundantURL(currentURL.String()) {
				log.Infof("Skipping URL as it's redundant: %s", currentURL)
				break
			}

			if r.isRedundantBody(currentURL.Host, resp, 97) {
				log.Infof("Skipping URL as similar content has been processed: %s", currentURL)
				break
			}

			r.Results <- Result{RequestURL: currentURL.String(), StatusCode: resp.StatusCode, Error: nil}

			if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
				location, err := resp.Location()
				if err != nil || location == nil {
					log.Warnf("Failed to handle redirect from %s", currentURL)
					break
				}
				currentURL = location
				redirectCount++
			} else {
				paths, err := r.scrape(resp)
				if err != nil {
					log.Warnf("Failed to scrape %s: %v", currentURL, err)
					break
				}

				rawURLs, err := r.setURL(currentURL.String(), paths)
				if err != nil {
					log.Warnf("Failed to set URLs from %s: %v", currentURL, err)
					break
				}

				for _, rawURL := range rawURLs {
					u, err := url.Parse(rawURL)
					if err != nil {
						log.Warnf("Error parsing URL %s: %v", rawURL, err)
						continue
					}
					go r.queueURL(c_queue, u)
				}
				break
			}
		}
	}
}

func (r *Runner) shouldAddRobotsTxt(c_url *url.URL) bool {
	return (c_url.Path == "" || c_url.Path == "/") && !strings.HasSuffix(c_url.Path, "robots.txt") && !r.isVisitedURL(c_url.String()+"/robots.txt")
}

func (r *Runner) addRobotsTxtToQueue(c_url *url.URL, c_queue chan<- *url.URL, c_wait chan<- int) {
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", c_url.Scheme, c_url.Host)
	robotsParsedURL, err := url.Parse(robotsURL)
	if err == nil {
		time.Sleep(r.getDelay() * time.Millisecond)
		c_wait <- 1
		go r.queueURL(c_queue, robotsParsedURL)
	}
}

func (r *Runner) isRedundantBody(host string, resp *http.Response, threshold int) bool {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Error reading response body: %v", err)
		return false
	}
	resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	hash, _ := ssdeep.FuzzyBytes(bodyBytes)

	if fuzzyHashes[host] == nil {
		fuzzyHashes[host] = make(map[string]bool)
	}

	for existingHash := range fuzzyHashes[host] {
		score, _ := ssdeep.Distance(existingHash, hash)

		if score >= threshold {
			return true
		}
	}

	fuzzyHashes[host][hash] = true
	return false
}

func (r *Runner) isRedundantURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.RawQuery == "" {
		if r.isVisitedURL(u.Path) {
			return true
		}
	} else {
		params, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return false
		}
		u.RawQuery = ""
		canonicalURL := u.String()
		if r.isVisitedURL(canonicalURL) {
			return true
		}
		aliasValues := make(map[string]string)
		for name, paramValues := range params {
			if len(paramValues) > 0 {
				aliasValues[name] = paramValues[0]
			}
		}
		foundAlias := false
		visitedURL.Range(func(key, value interface{}) bool {
			vURL, ok := key.(string)
			if !ok {
				return true
			}
			v, err := url.Parse(vURL)
			if err != nil {
				return true
			}
			if v.RawQuery == "" {
				if v.Path == u.Path && len(v.Query()) == len(params) {
					foundAlias = true
					return false
				}
			} else {
				vParams, err := url.ParseQuery(v.RawQuery)
				if err != nil {
					return true
				}
				v.RawQuery = ""
				vCanonicalURL := v.String()
				if vCanonicalURL == canonicalURL {
					foundAlias = true
					return false
				}
				vAliasValues := make(map[string]string)
				for name, paramValues := range vParams {
					if len(paramValues) > 0 {
						vAliasValues[name] = paramValues[0]
					}
				}
				alias := true
				for name, value := range aliasValues {
					if vAliasValues[name] != value {
						alias = false
						break
					}
				}
				if alias {
					foundAlias = true
					return false
				}
			}
			return true
		})
		if foundAlias {
			return true
		}
	}
	return false
}

func (r *Runner) request(u *url.URL) (req *http.Request, resp *http.Response, err error) {
	log.Debug("Requesting ", u.String())

	if r.isVisitedURL(u.String()) {
		log.Debugf("URL already visited: %s", u.String())
		return nil, nil, fmt.Errorf("URL already visited")
	}

	r.addVisitedURL(u.String())

	req, err = http.NewRequest("GET", u.String(), nil)
	if err != nil {
		log.Warnf("Failed to create request for %s: %v", u.String(), err)
		return
	}

	if r.Options.UserAgent != "" {
		req.Header.Add("User-Agent", r.Options.UserAgent)
	}

	r.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err = r.client.Do(req)
	if err != nil {
		log.Warnf("HTTP request failed for %s: %v", u.String(), err)
		return
	}

	return req, resp, nil
}

func (r *Runner) setURL(rawURL string, paths []string) (rawURLs []string, err error) {
	log.Debugf("Setting URL for %s", rawURL)

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	if urlutil.HasFileExtension(rawURL) {
		return nil, fmt.Errorf("URL has file extension")
	}

	for _, path := range paths {
		if r.shouldSkipPath(u, path) || strutil.IsBinaryString(path) || !strutil.IsPrintable(path) || strings.Count(u.Path, ".") >= 2 {
			continue
		}

		if domainutil.IsDomainName(path) {
			rawURLs = append(rawURLs, rawURL+"/"+path)
		}

		formattedURL := formatURL(u, path)
		normaizedURL, _ := r.normalizeURLString(formattedURL)
		rawURLs = append(rawURLs, normaizedURL)
	}

	return
}

func (r *Runner) shouldSkipPath(u *url.URL, path string) bool {
	return path == u.Host || r.isMedia(path) || path == "" || strings.HasSuffix(u.Host, path)
}

func formatURL(u *url.URL, path string) string {
	path = strings.ReplaceAll(path, "\\", "")

	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "http") && strings.Contains(path, ".") {
		path = "/" + path
	}

	if urlutil.HasScheme(path) || domainutil.IsValidDomain(path) || strings.HasPrefix(path, "//") {
		return path
	}

	if strings.ContainsAny(path, ".") {
		if strings.HasPrefix(path, "/") {
			return u.Scheme + "://" + u.Host + path
		} else {
			return u.String() + "/" + path
		}
	}

	if strings.HasPrefix(path, "/") && strings.ContainsAny(path, ".") {
		return u.Scheme + "://" + u.Host + path
	}

	if strings.HasPrefix(path, "/") {
		return u.Scheme + "://" + u.Host + path + "/"
	}

	if urlutil.HasFileExtension(path) || urlutil.HasParam(path) {
		return u.Scheme + "://" + u.Host + u.Path + "/" + path
	}

	return u.Scheme + "://" + u.Host + u.Path + "/" + path + "/"
}

func (r *Runner) scrape(resp *http.Response) ([]string, error) {
	log.Debugf("Scraping %s", resp.Request.URL.String())

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(resp.Request.URL.String(), "robots.txt") {
		return r.scrapeRobotsTxt(body), nil
	}

	return r.scrapePaths(body), nil
}

func (r *Runner) scrapeRobotsTxt(body []byte) []string {
	var res []string

	reExtension := regexp.MustCompile(`/\.[a-z0-9]+$`)

	matches := re_robots.FindAllStringSubmatch(string(body), -1)
	for _, match := range matches {
		path := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(match[1]), "*", ""), "$", "")
		path = strings.TrimSuffix(path, "?")
		path = reExtension.ReplaceAllString(path, "/")

		if len(path) > 1 {
			res = append(res, path)
		}
	}
	return sliceutil.Unique(res)
}

func (r *Runner) scrapePaths(body []byte) []string {
	var res []string
	matches := re_path.FindAllStringSubmatch(string(body), -1)
	for _, match := range matches {
		for _, path := range match {
			if path != "" {
				res = append(res, r.removeQuotes(path))
			}
		}
	}
	return sliceutil.Unique(res)
}

const normalizationFlags purell.NormalizationFlags = purell.FlagRemoveDefaultPort |
	purell.FlagLowercaseScheme |
	purell.FlagLowercaseHost |
	purell.FlagDecodeDWORDHost |
	purell.FlagDecodeOctalHost |
	purell.FlagDecodeHexHost |
	purell.FlagRemoveUnnecessaryHostDots |
	purell.FlagRemoveTrailingSlash |
	purell.FlagRemoveDotSegments |
	purell.FlagRemoveDuplicateSlashes |
	purell.FlagUppercaseEscapes |
	purell.FlagRemoveEmptyPortSeparator |
	purell.FlagDecodeUnnecessaryEscapes |
	purell.FlagRemoveTrailingSlash |
	purell.FlagEncodeNecessaryEscapes |
	purell.FlagSortQuery

func (r *Runner) normalizeURLString(rawURL string) (normalizedURL string, err error) {

	normalizedURL = strings.ReplaceAll(rawURL, `\`, `%5C`)
	normalizedURL, err = purell.NormalizeURLString(normalizedURL, normalizationFlags)
	return normalizedURL, err
}

func (r *Runner) isMedia(path string) bool {
	mimes := []string{"audio/", "application/", "font/", "image/", "multipart/", "text/", "video/"}
	for _, mime := range mimes {
		if strings.HasPrefix(path, mime) {
			return true
		}
	}
	return false
}

func (r *Runner) isTrapped(path string) bool {
	var tot int
	parts := strings.Split(path, "/")
	if len(parts) >= 10 {
		for _, part := range parts[1:] {
			if part != "" {
				tot += strings.Count(path, part)
			}
		}
		return tot/len(parts) >= 3
	}
	return false
}

func (r *Runner) getDelay() time.Duration {
	if r.Options.DelayJitter != 0 {
		return time.Duration(r.Options.Delay + rand.Intn(r.Options.DelayJitter))
	}
	return time.Duration(r.Options.Delay)
}

func (r *Runner) addVisitedURL(key string) {
	visitedURL.Store(key, true)
}

func (r *Runner) addVisitedHost(key string) {
	visitedHost.Store(key, true)
}

func (r *Runner) isVisitedURL(key string) bool {
	_, ok := visitedURL.Load(key)
	return ok
}

func (r *Runner) isVisitedHost(key string) bool {
	_, ok := visitedHost.Load(key)
	return ok
}

func (r *Runner) cleanURL(url string) string {
	url = urlutil.NormalizeSlashes(url)
	url = urlutil.EnsureHTTP(url)
	url = urlutil.EnsureTrailingSlash(url)
	return url
}

func (r *Runner) removeQuotes(input string) string {
	if len(input) < 2 {
		return input
	}

	if (input[0] == '"' && input[len(input)-1] == '"') || (input[0] == '\'' && input[len(input)-1] == '\'') {

		return input[1 : len(input)-1]
	}
	return input
}

func (r *Runner) setLogLevel() {
	if r.Options.Verbose == 1 {
		log.SetLevel(log.InfoLevel)
	} else if r.Options.Verbose == 2 {
		log.SetLevel(log.DebugLevel)
	} else if r.Options.Silence {
		log.SetLevel(log.FatalLevel)
	} else {
		log.SetLevel(log.ErrorLevel)
	}
}
