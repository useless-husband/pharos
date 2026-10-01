package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// startTarget runs the monitored endpoints /svc/1 to /svc/N in a child
// process, so that their CPU time, TLS handshakes above all, is not counted
// as Pharos's. The first hang of them never answer, like hosts behind a
// failed network link: the check waits for its full timeout.
func startTarget(hang int, useTLS bool) (url string, stop func()) {
	cmd := exec.Command(os.Args[0], "-serve-target", "-target-hang", strconv.Itoa(hang), "-tls="+strconv.FormatBool(useTLS))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe() // the child exits when this closes, even if we crash
	must(err)
	out, err := cmd.StdoutPipe()
	must(err)
	must(cmd.Start())
	line, err := bufio.NewReader(out).ReadString('\n')
	must(err)
	return strings.TrimSpace(line), func() {
		stdin.Close()
		_ = cmd.Wait()
	}
}

// serveTarget is the child process: it prints its URL and serves until its
// standard input closes.
func serveTarget(hang int, useTLS bool) {
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	scheme := "http"
	if useTLS {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{selfSigned()}})
		scheme = "https"
	}
	fmt.Printf("%s://%s\n", scheme, ln.Addr())
	must(http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/svc/%d", &n); err == nil && n <= hang {
			<-r.Context().Done()
			return
		}
		time.Sleep(time.Duration(2+mrand.IntN(20)) * time.Millisecond)
		fmt.Fprint(w, `{"status":"ok"}`)
	})))
}

// selfSigned makes an ECDSA P-256 certificate for 127.0.0.1, the key type
// most public sites use.
func selfSigned() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "loadtest"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
