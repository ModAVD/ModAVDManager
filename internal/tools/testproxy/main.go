// Command testproxy runs internal/appproxy/testproxy on the host, for trying the per-app proxy in a real instance
// (the guest reaches it at 10.0.2.2:<port>). Neither exe imports it.
//
//	go run ./internal/tools/testproxy -mode socks5 -log proxy.ndjson
//
// It listens on 127.0.0.1 only, on a free port it picks (printed on stderr) or on -port. Ports that adb, the
// emulators and common proxy programs use are refused, so it never stands in for the user's own proxy. Every
// CONNECT and UDP datagram is one JSON line in -log (stdout when not given). It runs until killed.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"modavd/internal/appproxy/testproxy"
)

// reserved are ports it does not take: adb, the emulators' consoles and adb (20000-20099 here), proxy programs.
func reserved(p int) bool {
	switch p {
	case 1080, 5037, 7890, 7891, 7897, 10808, 10809:
		return true
	}
	return p >= 20000 && p <= 20099
}

func main() {
	mode := flag.String("mode", "socks5", "socks5, socks5-noudp or http")
	port := flag.Int("port", 0, "port on 127.0.0.1 (0: pick a free one)")
	logPath := flag.String("log", "", "JSON lines file (default stdout)")
	flag.Parse()
	if reserved(*port) {
		fmt.Fprintf(os.Stderr, "port %d is reserved (adb, emulators, proxy programs)\n", *port)
		os.Exit(2)
	}
	var w io.Writer = os.Stdout
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}
	var p *testproxy.Proxy
	for {
		var err error
		p, err = testproxy.Start(testproxy.Options{Mode: testproxy.Mode(*mode), Listen: fmt.Sprintf("127.0.0.1:%d", *port), Log: w})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *port != 0 || !reserved(int(p.Addr().Port())) {
			break
		}
		p.Close()
	}
	fmt.Fprintf(os.Stderr, "testproxy %s listening on %s (guest: 10.0.2.2:%d)\n", *mode, p.Addr(), p.Addr().Port())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	p.Close()
}
