package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {

	var domains []string

	var dates bool
	flag.BoolVar(&dates, "dates", false, "show date of fetch in the first column")

	var noSubs bool
	flag.BoolVar(&noSubs, "no-subs", false, "don't include subdomains of the target domain")

	var getVersionsFlag bool
	flag.BoolVar(&getVersionsFlag, "get-versions", false, "list URLs for crawled versions of input URL(s)")

	var sourcesFlag string
	flag.StringVar(&sourcesFlag, "sources", "wayback,commoncrawl,virustotal", "comma-separated list of sources to query: wayback, commoncrawl, virustotal")

	var outputFilePath string
	flag.StringVar(&outputFilePath, "output", "", "output file path (default: stdout)")

	var concurrency int
	flag.IntVar(&concurrency, "concurrency", 5, "number of concurrent requests")

	var timeout int
	flag.IntVar(&timeout, "timeout", 10, "HTTP request timeout in seconds")

	flag.Parse()

	var outputFile *os.File
	if outputFilePath != "" {
		var err error
		outputFile, err = os.Create(outputFilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create output file: %s\n", err)
			os.Exit(1)
		}
		defer outputFile.Close()
	} else {
		outputFile = os.Stdout
	}

	// Initialize the global HTTP client with a timeout
	httpClient = &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	if flag.NArg() > 0 {
		// fetch for a single domain
		domains = []string{flag.Arg(0)}
	} else {

		// fetch for all domains from stdin
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			domains = append(domains, sc.Text())
		}

		if err := sc.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to read input: %s\n", err)
		}
	}

	// get-versions mode
	if getVersionsFlag {

		for _, u := range domains {
			versions, err := getVersions(u)
			if err != nil {
				continue
			}
			fmt.Fprintln(outputFile, strings.Join(versions, "\n"))
		}

		return
	}

	// Determine which sources to use
	sources := make(map[string]bool)
	for _, s := range strings.Split(sourcesFlag, ",") {
		sources[strings.TrimSpace(s)] = true
	}

	var fetchFns []fetchFn
	if sources["wayback"] {
		fetchFns = append(fetchFns, getWaybackURLs)
	}
	if sources["commoncrawl"] {
		fetchFns = append(fetchFns, getCommonCrawlURLs)
	}
	if sources["virustotal"] {
		fetchFns = append(fetchFns, getVirusTotalURLs)
	}

	if len(fetchFns) == 0 {
		fmt.Fprintf(os.Stderr, "no valid sources specified. Please choose from: wayback, commoncrawl, virustotal\n")
		os.Exit(1)
	}

	for _, domain := range domains {

		var wg sync.WaitGroup
		wurls := make(chan wurl)
		limiter := make(chan struct{}, concurrency) // Concurrency limiter

		for _, fn := range fetchFns {
			wg.Add(1)
			fetch := fn
			go func() {
				defer wg.Done()
				limiter <- struct{}{} // Acquire a token
				resp, err := fetch(domain, noSubs)
				<-limiter // Release the token
				if err != nil {
					return
				}
				for _, r := range resp {
					if noSubs && isSubdomain(r.url, domain) {
						continue
					}
					wurls <- r
				}
			}()
		}

		go func() {
			wg.Wait()
			close(wurls)
		}()

		seen := make(map[string]bool)
		for w := range wurls {
			if _, ok := seen[w.url]; ok {
				continue
			}
			seen[w.url] = true

			if dates {

				d, err := time.Parse("20060102150405", w.date)
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to parse date [%s] for URL [%s]\n", w.date, w.url)
				}

				fmt.Fprintf(outputFile, "%s %s\n", d.Format(time.RFC3339), w.url)

			} else {
				fmt.Fprintln(outputFile, w.url)
			}
		}
	}

}

type wurl struct {
	date string
	url  string
}

type fetchFn func(string, bool) ([]wurl, error)

func getWaybackURLs(domain string, noSubs bool) ([]wurl, error) {
	subsWildcard := "*."
	if noSubs {
		subsWildcard = ""
	}

	// Use the global httpClient
	res, err := httpClient.Get(
		fmt.Sprintf("http://web.archive.org/cdx/search/cdx?url=%s%s/*&output=json&collapse=urlkey", subsWildcard, domain),
	)
	if err != nil {
		return []wurl{}, err
	}

	raw, err := ioutil.ReadAll(res.Body)

	res.Body.Close()
	if err != nil {
		return []wurl{}, err
	}

	var wrapper [][]string
	err = json.Unmarshal(raw, &wrapper)

	out := make([]wurl, 0, len(wrapper))

	skip := true
	for _, urls := range wrapper {
		// The first item is always just the string "original",
		// so we should skip the first item
		if skip {
			skip = false
			continue
		}
		out = append(out, wurl{date: urls[1], url: urls[2]})
	}

	return out, nil

}

func getCommonCrawlURLs(domain string, noSubs bool) ([]wurl, error) {
	subsWildcard := "*."
	if noSubs {
		subsWildcard = ""
	}

	// Use the global httpClient
	res, err := httpClient.Get(
		fmt.Sprintf("http://index.commoncrawl.org/CC-MAIN-2018-22-index?url=%s%s/*&output=json", subsWildcard, domain),
	)
	if err != nil {
		return []wurl{}, err
	}

	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)

	out := make([]wurl, 0)

	for sc.Scan() {

		wrapper := struct {
			URL       string `json:"url"`
			Timestamp string `json:"timestamp"`
		}{}
		err = json.Unmarshal([]byte(sc.Text()), &wrapper)

		if err != nil {
			continue
		}

		out = append(out, wurl{date: wrapper.Timestamp, url: wrapper.URL})
	}

	return out, nil

}

// Declare httpClient globally
var httpClient *http.Client

func getVirusTotalURLs(domain string, noSubs bool) ([]wurl, error) {
	out := make([]wurl, 0)

	apiKey := os.Getenv("VT_API_KEY")
	if apiKey == "" {
		// no API key isn't an error,
		// just don't fetch
		return out, nil
	}

	fetchURL := fmt.Sprintf(
		"https://www.virustotal.com/vtapi/v2/domain/report?apikey=%s&domain=%s",
		apiKey,
		domain,
	)

	// Use the global httpClient
	resp, err := httpClient.Get(fetchURL)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	wrapper := struct {
		URLs []struct {
			URL string `json:"url"`
			// TODO: handle VT date format (2018-03-26 09:22:43)
			//Date string `json:"scan_date"`
		} `json:"detected_urls"`
	}{}

	dec := json.NewDecoder(resp.Body)

	err = dec.Decode(&wrapper)

	for _, u := range wrapper.URLs {
		out = append(out, wurl{url: u.URL})
	}

	return out, nil

}

func isSubdomain(rawUrl, domain string) bool {
	u, err := url.Parse(rawUrl)
	if err != nil {
		// we can't parse the URL so just
		// err on the side of including it in output
		return false
	}

	return strings.ToLower(u.Hostname()) != strings.ToLower(domain)
}

func getVersions(u string) ([]string, error) {
	out := make([]string, 0)

	// Use the global httpClient
	resp, err := httpClient.Get(fmt.Sprintf(
		"http://web.archive.org/cdx/search/cdx?url=%s&output=json", u,
	))

	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	r := [][]string{}

	dec := json.NewDecoder(resp.Body)

	err = dec.Decode(&r)
	if err != nil {
		return out, err
	}

	first := true
	seen := make(map[string]bool)
	for _, s := range r {

		// skip the first element, it's the field names
		if first {
			first = false
			continue
		}

		// fields: "urlkey", "timestamp", "original", "mimetype", "statuscode", "digest", "length"
		if seen[s[5]] {
			continue
		}
		seen[s[5]] = true
		out = append(out, fmt.Sprintf("https://web.archive.org/web/%sif_/%s", s[1], s[2]))
	}

	return out, nil
}
