package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

//
// Options - query options
//
type Options struct {
	qopts        QueryOptions
	useV6        bool
	useV4        bool
	resolver     net.IP
	masterIP     net.IP
	masterName   string
	additional   string
	noqueryns    bool
	masterSerial uint32
	delta        int
}

// Defaults
var (
	defaultTimeout     = 3
	defaultRetries     = 3
	defaultSerialDelta = 0
)

//
// Request - request parameters
//
type Request struct {
	nsname string
	nsip   net.IP
}

//
// Response - response information
//
type Response struct {
	nsname string
	nsip   net.IP
	serial uint32
	err    error
}

// For goroutine communications and synchronization
var wg sync.WaitGroup
var numParallel uint16 = 20
var tokens = make(chan struct{}, int(numParallel))
var results = make(chan *Response)

func getFirstIPv4Address(hostname string, opts Options) net.IP {

	var ip net.IP

	opts.qopts.rdflag = true

	response, err := SendQuery(hostname, dns.TypeA, opts.resolver, opts.qopts)
	if err == nil {
		for _, rr := range response.Answer {
			if rr.Header().Rrtype == dns.TypeA {
				return rr.(*dns.A).A
			}
		}
	}

	return ip
}

func getIPAddresses(hostname string, rrtype uint16, opts Options) []net.IP {

	var ipList []net.IP

	opts.qopts.rdflag = true

	switch rrtype {
	case dns.TypeAAAA, dns.TypeA:
		response, err := SendQuery(hostname, rrtype, opts.resolver, opts.qopts)
		if err != nil || response == nil {
			break
		}
		for _, rr := range response.Answer {
			if rr.Header().Rrtype == rrtype {
				if rrtype == dns.TypeAAAA {
					ipList = append(ipList, rr.(*dns.AAAA).AAAA)
				} else if rrtype == dns.TypeA {
					ipList = append(ipList, rr.(*dns.A).A)
				}
			}
		}
	default:
		fmt.Printf("getIPAddresses: %d: invalid rrtype\n", rrtype)
	}

	return ipList

}

func getSerial(zone string, ip net.IP, opts Options) (serial uint32, err error) {

	var response *dns.Msg

	opts.qopts.rdflag = false

	response, err = SendQuery(zone, dns.TypeSOA, ip, opts.qopts)
	if err != nil {
		return serial, err
	}
	switch response.MsgHdr.Rcode {
	case dns.RcodeSuccess:
		break
	case dns.RcodeNameError:
		return serial, fmt.Errorf("NXDOMAIN: %s: name doesn't exist", zone)
	default:
		return serial, fmt.Errorf("Error: Response code: %s",
			dns.RcodeToString[response.MsgHdr.Rcode])
	}

	for _, rr := range response.Answer {
		if rr.Header().Rrtype == dns.TypeSOA {
			return rr.(*dns.SOA).Serial, nil
		}
	}

	return serial, fmt.Errorf("SOA record not found at %s",
		ip.String())
}

func getSerialAsync(zone string, ip net.IP, nsName string, opts Options) {

	defer wg.Done()

	serial, err := getSerial(zone, ip, opts)
	<-tokens // Release token

	r := new(Response)
	r.nsip = ip
	r.nsname = nsName
	r.serial = serial
	r.err = err
	results <- r
}

func getNSnames(zone string, opts Options) []string {

	var nsNameList []string

	opts.qopts.rdflag = true
	response, err := SendQuery(zone, dns.TypeNS, opts.resolver, opts.qopts)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	if response.MsgHdr.Rcode == dns.RcodeNameError {
		fmt.Printf("Error: %s doesn't exist\n", zone)
		os.Exit(1)
	}

	for _, rr := range response.Answer {
		if rr.Header().Rrtype == dns.TypeNS {
			nsNameList = append(nsNameList, rr.(*dns.NS).Ns)
		}
	}

	return nsNameList
}

func getRequests(nsNameList []string, opts Options) []*Request {

	var ip net.IP
	var aList []net.IP
	var requests []*Request
	var r *Request

	sort.Strings(nsNameList)

	for _, nsName := range nsNameList {
		ip = net.ParseIP(nsName)
		if ip != nil {
			r = new(Request)
			r.nsname = nsName
			r.nsip = ip
			requests = append(requests, r)
			continue
		}
		aList = make([]net.IP, 0)
		if !opts.useV4 {
			aList = append(aList,
				getIPAddresses(nsName, dns.TypeAAAA, opts)...)
		}
		if !opts.useV6 {
			aList = append(aList,
				getIPAddresses(nsName, dns.TypeA, opts)...)
		}
		for _, ip := range aList {
			r = new(Request)
			r.nsname = nsName
			r.nsip = ip
			requests = append(requests, r)
		}
	}

	return requests

}

func printMasterSerial(zone string, popts *Options) {

	var err error

	if popts.masterIP == nil {
		popts.masterIP = getFirstIPv4Address(popts.masterName, *popts)
		if popts.masterIP == nil {
			fmt.Printf("Error: couldn't resolve master name: %s\n", popts.masterName)
			os.Exit(1)
		}
	} else {
		popts.masterName = popts.masterIP.String()
	}

	popts.masterSerial, err = getSerial(zone, popts.masterIP, *popts)
	if err == nil {
		fmt.Printf("%15d [%9s] %s %s\n", popts.masterSerial, "MASTER",
			popts.masterName, popts.masterIP)
	} else {
		fmt.Printf("Error: %s %s: couldn't obtain serial: %s\n",
			popts.masterName, popts.masterIP, err.Error())
		os.Exit(1)
	}

}

func printResult(r *Response, opts Options) bool {

	if r.err == nil {
		if opts.masterIP != nil {
			delta := int(opts.masterSerial) - int(r.serial)
			fmt.Printf("%15d [%9d] %s %s\n", r.serial, delta, r.nsname, r.nsip)
			if delta < 0 {
				delta = -delta
			}
			if delta > opts.delta {
				return false
			}
		} else {
			fmt.Printf("%15d %s %s\n", r.serial, r.nsname, r.nsip)
		}
		return true
	}

	fmt.Printf("Error: %s %s: couldn't obtain serial: %s\n",
		r.nsname, r.nsip, r.err.Error())
	return false

}

func getAdditionalServers(opts Options) []string {

	var s []string
	var ip net.IP

	s0 := strings.Split(opts.additional, ",")

	for _, x := range s0 {
		ip = net.ParseIP(x)
		if ip != nil {
			s = append(s, x)
		} else {
			s = append(s, dns.Fqdn(x))
		}
	}

	return s
}

func doFlags() (string, Options) {

	var opts Options

	help := flag.Bool("h", false, "print help string")
	flag.BoolVar(&opts.useV6, "6", false, "use IPv6 only")
	flag.BoolVar(&opts.useV4, "4", false, "use IPv4 only")
	master := flag.String("m", "", "master server address")
	flag.StringVar(&opts.additional, "a", "", "additional nameservers: n1,n2..")
	flag.BoolVar(&opts.noqueryns, "n", false, "don't query advertised nameservers")
	flag.IntVar(&opts.delta, "d", defaultSerialDelta, "allowed serial number drift")
	timeoutp := flag.Int("t", defaultTimeout, "query timeout in seconds")
	opts.qopts.timeout = time.Second * time.Duration(*timeoutp)
	flag.IntVar(&opts.qopts.retries, "r", defaultRetries, "number of query retries")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [Options] <zone>

	Options:
	-h          Print this help string
	-4          Use IPv4 transport only
	-6          Use IPv6 transport only
	-t N        Query timeout value in seconds (default %d)
	-r N        Maximum # SOA query retries for each server (default %d)
	-d N        Allowed SOA serial number drift (default %d)
	-m ns       Master server name/address to compare serial numbers with
	-a ns1,..   Specify additional nameserver names/addresses to query
	-n          Don't query advertised nameservers for the zone
`, path.Base(os.Args[0]), defaultTimeout, defaultRetries, defaultSerialDelta)
	}

	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(1)
	}

	if *master != "" {
		opts.masterIP = net.ParseIP(*master)
		if opts.masterIP == nil { // assume hostname
			opts.masterName = dns.Fqdn(*master)
		}
	}

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "Incorrect number of arguments.\n")
		flag.Usage()
		os.Exit(1)
	}
	args := flag.Args()
	return dns.Fqdn(args[0]), opts
}

func main() {

	var err error
	var nsNameList []string
	var requests []*Request

	zone, opts := doFlags()

	opts.resolver, err = GetResolver()
	if err != nil {
		fmt.Printf("Error getting resolver: %s\n", err.Error())
		os.Exit(1)
	}

	if opts.additional != "" {
		nsNameList = getAdditionalServers(opts)
	}
	if !opts.noqueryns {
		nsNameList = append(nsNameList, getNSnames(zone, opts)...)
	}
	requests = getRequests(nsNameList, opts)

	opts.qopts.rdflag = false

	fmt.Printf("Zone: %s\n", zone)
	if opts.masterIP != nil || opts.masterName != "" {
		printMasterSerial(zone, &opts)
	}

	go func() {
		for _, x := range requests {
			wg.Add(1)
			tokens <- struct{}{}
			go getSerialAsync(zone, x.nsip, x.nsname, opts)
		}
		wg.Wait()
		close(results)
	}()

	rc := 0
	for r := range results {
		if !printResult(r, opts) {
			rc = 2
		}
	}
	os.Exit(rc)
}
