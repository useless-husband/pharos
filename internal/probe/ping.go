package probe

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/model"
)

// pingCount is how many echo requests one check sends. The check passes if
// any reply arrives; loss is reported in the message.
const pingCount = 3

// pingProber sends ICMP echo requests. It uses unprivileged ICMP sockets
// ("udp4"/"udp6"), which macOS allows by default and Linux allows when
// net.ipv4.ping_group_range covers the process's group. It falls back to raw
// sockets, which need root or CAP_NET_RAW.
type pingProber struct{ m config.Monitor }

func (p *pingProber) Probe(ctx context.Context) model.Check {
	m := p.m
	ctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
	defer cancel()
	start := time.Now()

	ip, err := resolveIP(ctx, m.Address, m.IPVersion)
	if err != nil {
		return down(describe(err, m.Timeout.D()), time.Since(start))
	}
	v6 := ip.To4() == nil
	conn, privileged, err := listenICMP(v6)
	if err != nil {
		return down("ping: "+err.Error(), time.Since(start))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var dst net.Addr = &net.UDPAddr{IP: ip}
	if privileged {
		dst = &net.IPAddr{IP: ip}
	}
	proto, reqType, replyType := 1, icmp.Type(ipv4.ICMPTypeEcho), icmp.Type(ipv4.ICMPTypeEchoReply)
	if v6 {
		proto, reqType, replyType = 58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}

	id := rand.IntN(0xffff)
	payload := []byte(fmt.Sprintf("pharos-%d", rand.Uint64()))
	sent := map[int]time.Time{}
	var rtts []time.Duration
	buf := make([]byte, 1500)

	for seq := 1; seq <= pingCount; seq++ {
		msg := icmp.Message{Type: reqType, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload}}
		b, err := msg.Marshal(nil)
		if err != nil {
			return down("ping: "+err.Error(), time.Since(start))
		}
		sent[seq] = time.Now()
		if _, err := conn.WriteTo(b, dst); err != nil {
			return down("ping: "+describe(err, m.Timeout.D()), time.Since(start))
		}
		// Collect replies until the next send is due. After the last send,
		// wait up to 1s for stragglers, or until the deadline if nothing has
		// come back yet.
		wait := time.Now().Add(250 * time.Millisecond)
		if seq == pingCount {
			wait = deadlineOf(ctx)
			if len(rtts) > 0 {
				wait = minTime(wait, time.Now().Add(time.Second))
			}
		}
		for len(sent) > 0 && time.Now().Before(wait) {
			_ = conn.SetReadDeadline(minTime(wait, deadlineOf(ctx)))
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			reply, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil || reply.Type != replyType {
				continue
			}
			echo, ok := reply.Body.(*icmp.Echo)
			// Unprivileged sockets rewrite the ID, so match on payload.
			if !ok || string(echo.Data) != string(payload) {
				continue
			}
			if t, ok := sent[echo.Seq]; ok {
				rtts = append(rtts, time.Since(t))
				delete(sent, echo.Seq)
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(rtts) == 0 {
		return down(fmt.Sprintf("no reply from %s (%d packets sent)", ip, pingCount), time.Since(start))
	}
	var sum time.Duration
	for _, r := range rtts {
		sum += r
	}
	c := model.Check{Status: model.StatusUp, Latency: sum / time.Duration(len(rtts))}
	if lost := pingCount - len(rtts); lost > 0 {
		c.Message = fmt.Sprintf("%d of %d packets lost", lost, pingCount)
	}
	return applyLatency(c, m.Expect.MaxLatency.D())
}

func deadlineOf(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(5 * time.Second)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func listenICMP(v6 bool) (*icmp.PacketConn, bool, error) {
	udp, raw, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		udp, raw, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	c, err := icmp.ListenPacket(udp, addr)
	if err == nil {
		return c, false, nil
	}
	c, err2 := icmp.ListenPacket(raw, addr)
	if err2 == nil {
		return c, true, nil
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err2, os.ErrPermission) {
		hint := "run as root or grant CAP_NET_RAW"
		if runtime.GOOS == "linux" {
			hint = `allow unprivileged ping with sysctl net.ipv4.ping_group_range="0 2147483647", or grant CAP_NET_RAW`
		}
		return nil, false, fmt.Errorf("not permitted to send ICMP (%s)", hint)
	}
	return nil, false, err
}

func resolveIP(ctx context.Context, host, version string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	fam := "ip"
	if version == "4" || version == "6" {
		fam += version
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, fam, host)
	if err != nil {
		return nil, err
	}
	// Prefer IPv4 unless IPv6 was requested: it is what most people expect.
	for _, ip := range ips {
		if ip.To4() != nil || version == "6" {
			return ip, nil
		}
	}
	return ips[0], nil
}
