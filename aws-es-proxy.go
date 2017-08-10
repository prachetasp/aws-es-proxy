package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/aws/signer/v4"
)

type proxy struct {
	Scheme               string
	Host                 string
	Region               string
	Service              string
	Verbose              bool
	Prettify             bool
	Refresh              float64
	CredentialsLastUpped time.Time
	Credentials          *credentials.Credentials
}

func getSigner(p *proxy) *v4.Signer {
	now := time.Now()
	diff := now.Sub(p.CredentialsLastUpped)
	if p.Credentials == nil || diff.Seconds() > p.Refresh {
		p.Credentials, p.CredentialsLastUpped = getCredentials()
	}

	return v4.NewSigner(p.Credentials)
}

func getCredentials() (*credentials.Credentials, time.Time) {
	sess := session.Must(session.NewSession())
	Credentials := sess.Config.Credentials
	log.Print("Generated fresh AWS Credentials object")
	return Credentials, time.Now()
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func replaceBody(req *http.Request) []byte {
	if req.Body == nil {
		return []byte{}
	}
	payload, _ := ioutil.ReadAll(req.Body)
	req.Body = ioutil.NopCloser(bytes.NewReader(payload))
	return payload
}

func parseEndpoint(endpoint string, p *proxy) {
	link, err := url.Parse(endpoint)
	if err != nil {
		log.Fatalf("ERROR: Failed parsing endpoint: %s\n", endpoint)
	}

	// Only http/https are supported schemes
	scheme := func(x string) string {
		switch x {
		case "http", "https":
			return x
		}
		return "https"
	}
	link.Scheme = scheme(link.Scheme)

	// Unkown schemes sometimes result in empty host value
	if link.Host == "" {
		log.Fatalf("ERROR: Empty host information in submitted endpoint (%s)\n", endpoint)
	}

	// Extract region and service from link
	parts := strings.Split(link.Host, ".")
	var region, service string

	if len(parts) == 5 {
		region, service = parts[1], parts[2]
	} else {
		log.Fatalln("ERROR: Submitted endpoint is not a valid Amazon ElasticSearch Endpoint")
	}

	// Build proxy struct
	p.Scheme = link.Scheme
	p.Host = link.Host
	p.Region = region
	p.Service = service

}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestStarted := time.Now()
	dump, err := httputil.DumpRequest(r, true)
	defer r.Body.Close()

	respondError := func(err error) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
	}

	endpoint := *r.URL
	endpoint.Host = p.Host
	endpoint.Scheme = p.Scheme

	req, err := http.NewRequest(r.Method, endpoint.String(), r.Body)
	if err != nil {
		respondError(err)
		return
	}

	// Workaround for ES 5.1 and Kibana 5.1.1
	if val, ok := r.Header["Kbn-Version"]; ok {
		req.Header.Set("Kbn-Version", val[0])
	}

	// Start AWS session from ENV, Shared Creds or EC2Role
	signer := getSigner(p)

	// Sign the request with AWSv4
	payload := bytes.NewReader(replaceBody(req))
	signer.Sign(req, payload, p.Service, p.Region, time.Now())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println(err)
		respondError(err)
		return
	}

	defer resp.Body.Close()

	// Write back received headers
	copyHeaders(w.Header(), resp.Header)

	buf := bytes.Buffer{}
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		log.Fatal(err)
	}

	// Send response back
	w.WriteHeader(resp.StatusCode)
	w.Write(buf.Bytes())

	// Log everything
	remoteAddr := r.RemoteAddr
	rawQuery := string(dump)
	rawQuery = strings.Replace(rawQuery, "\n", " ", -1)
	regex, _ := regexp.Compile("{.*}")
	regEx, _ := regexp.Compile("_msearch|_bulk")
	queryEx := regEx.FindString(rawQuery)

	var query string

	if len(queryEx) == 0 {
		query = regex.FindString(rawQuery)
	} else {
		query = ""
	}

	if p.Verbose {
		requestEnded := time.Since(requestStarted)

		if p.Prettify {
			var prettyBody bytes.Buffer
			json.Indent(&prettyBody, []byte(query), "", "  ")
			t := time.Now()

			fmt.Println()
			fmt.Println("========================")
			fmt.Println(t.Format("2006/01/02 15:04:05"))
			fmt.Println("Remote Address: ", remoteAddr)
			fmt.Println("Request URI: ", endpoint.RequestURI())
			fmt.Println("Method: ", r.Method)
			fmt.Println("Status: ", resp.StatusCode)
			fmt.Printf("Took: %.3fs\n", requestEnded.Seconds())
			fmt.Println("Body: ")
			fmt.Println(string(prettyBody.Bytes()))
			fmt.Println("========================")

		} else {
			log.Printf(" -> %s; %s; %s; %s; %d; %.3fs\n",
				r.Method, remoteAddr, endpoint.RequestURI(), query, resp.StatusCode, requestEnded.Seconds())
		}
	}
}

func main() {
	var endpoint, listenAddress string
	var verbose bool
	var prettify bool
	var refresh float64

	// TODO: Use a more sophisticated args parser that can enforce arguments
	flag.StringVar(&endpoint, "endpoint", "", "Amazon ElasticSearch Endpoint (e.g: https://dummy-host.eu-west-1.es.amazonaws.com)")
	flag.StringVar(&listenAddress, "listen", "127.0.0.1:9200", "Local TCP port to listen on")
	flag.BoolVar(&verbose, "verbose", false, "Print user requests")
	flag.BoolVar(&prettify, "pretty", false, "Prettify verbose output")
	flag.Float64Var(&refresh, "refresh", 120, "Refresh AWS Credentials Automatically every XX seconds")

	flag.Parse()

	if len(os.Args) < 3 {
		fmt.Println("You need to specify Amazon ElasticSearch endpoint.")
		fmt.Println("Please run with '-h' for a list of available arguments.")
		os.Exit(1)
	}

	mux := &proxy{Verbose: verbose, Prettify: prettify, Refresh: refresh}
	parseEndpoint(endpoint, mux)

	fmt.Printf("Listening on %s\n", listenAddress)
	log.Fatal(http.ListenAndServe(listenAddress, mux))
}
