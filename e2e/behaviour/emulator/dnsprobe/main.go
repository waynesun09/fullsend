// Command dnsprobe times name lookups and one emulator API call the way
// fullsend's fetch makes them, to find where a slow harness load goes.
//
//	dnsprobe HOST URL TOKEN
//
// Prints one line: the pure-Go resolver's LookupIPAddr time (what fullsend's
// fetch guard calls), the cgo resolver's if built with cgo, and the URL's
// GET time with a fresh transport, as fetch builds one per file.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func lookup(r *net.Resolver, host string) string {
	start := time.Now()
	addrs, err := r.LookupIPAddr(context.Background(), host)
	d := time.Since(start)
	if err != nil {
		return fmt.Sprintf("%.3fs err=%v", d.Seconds(), err)
	}
	return fmt.Sprintf("%.3fs %v", d.Seconds(), addrs)
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: dnsprobe HOST URL TOKEN")
		os.Exit(2)
	}
	host, url, token := os.Args[1], os.Args[2], os.Args[3]
	pure := lookup(&net.Resolver{PreferGo: true}, host)
	sys := lookup(net.DefaultResolver, host)

	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "token "+token)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 60 * time.Second}
	status := "err"
	resp, err := client.Do(req)
	if err == nil {
		status = resp.Status
		resp.Body.Close()
	} else {
		status = err.Error()
	}
	fmt.Printf("%s go=%s default=%s get=%.3fs %s\n",
		time.Now().UTC().Format("15:04:05"), pure, sys, time.Since(start).Seconds(), status)
}
