package dns

import (
	"net/netip"
	"strings"
	"time"

	"github.com/Dreamacro/clash/common/cache"
	"github.com/Dreamacro/clash/common/nnip"
	"github.com/Dreamacro/clash/component/fakeip"
	"github.com/Dreamacro/clash/component/trie"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/context"
	"github.com/Dreamacro/clash/log"

	D "github.com/miekg/dns"
)

type (
	handler    func(ctx *context.DNSContext, r *D.Msg) (*D.Msg, error)
	middleware func(next handler) handler
)

func withHosts(hosts *trie.DomainTrie[netip.Addr], mapping *cache.LruCache[netip.Addr, string]) middleware {
	return func(next handler) handler {
		return func(ctx *context.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			if !isIPRequest(q) {
				return next(ctx, r)
			}

			host := strings.TrimRight(q.Name, ".")

			record := hosts.Search(host)
			if record == nil {
				return next(ctx, r)
			}

			ip := record.Data()
			msg := r.Copy()

			if ip.Is4() && q.Qtype == D.TypeA {
				rr := &D.A{}
				rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 10}
				rr.A = ip.AsSlice()

				msg.Answer = []D.RR{rr}
			} else if q.Qtype == D.TypeAAAA {
				rr := &D.AAAA{}
				rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeAAAA, Class: D.ClassINET, Ttl: 10}
				ip := ip.As16()
				rr.AAAA = ip[:]
				msg.Answer = []D.RR{rr}
			} else {
				return next(ctx, r)
			}

			if mapping != nil {
				mapping.SetWithExpire(ip, host, time.Now().Add(time.Second*10))
			}

			ctx.SetType(context.DNSTypeHost)
			msg.SetRcode(r, D.RcodeSuccess)
			msg.Authoritative = true
			msg.RecursionAvailable = true

			return msg, nil
		}
	}
}

func withMapping(mapping *cache.LruCache[netip.Addr, string]) middleware {
	return func(next handler) handler {
		return func(ctx *context.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			if !isIPRequest(q) {
				return next(ctx, r)
			}

			msg, err := next(ctx, r)
			if err != nil {
				return nil, err
			}

			host := strings.TrimRight(q.Name, ".")

			for _, ans := range msg.Answer {
				var ip netip.Addr
				var ttl uint32

				switch a := ans.(type) {
				case *D.A:
					ip = nnip.IpToAddr(a.A)
					ttl = a.Hdr.Ttl
				case *D.AAAA:
					ip = nnip.IpToAddr(a.AAAA)
					ttl = a.Hdr.Ttl
				default:
					continue
				}

				mapping.SetWithExpire(ip, host, time.Now().Add(time.Second*time.Duration(ttl)))
			}

			return msg, nil
		}
	}
}

func withFakeIP(fakePool *fakeip.Pool) middleware {
	return func(next handler) handler {
		return func(ctx *context.DNSContext, r *D.Msg) (*D.Msg, error) {
			q := r.Question[0]

			host := strings.TrimRight(q.Name, ".")
			if fakePool.ShouldSkipped(host) {
				return next(ctx, r)
			}

			switch q.Qtype {
			case D.TypeAAAA, D.TypeSVCB, D.TypeHTTPS:
				return handleMsgWithEmptyAnswer(r), nil
			}

			if q.Qtype != D.TypeA {
				return next(ctx, r)
			}

			rr := &D.A{}
			rr.Hdr = D.RR_Header{Name: q.Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: dnsDefaultTTL}
			ip := fakePool.Lookup(host)
			rr.A = ip.AsSlice()
			msg := r.Copy()
			msg.Answer = []D.RR{rr}

			ctx.SetType(context.DNSTypeFakeIP)
			setMsgTTL(msg, 1)
			msg.SetRcode(r, D.RcodeSuccess)
			msg.Authoritative = true
			msg.RecursionAvailable = true

			return msg, nil
		}
	}
}

func withResolver(resolver *Resolver) handler {
	return func(ctx *context.DNSContext, r *D.Msg) (*D.Msg, error) {
		ctx.SetType(context.DNSTypeRaw)
		q := r.Question[0]

		// return a empty AAAA msg when ipv6 disabled
		if !resolver.ipv6 && q.Qtype == D.TypeAAAA {
			return handleMsgWithEmptyAnswer(r), nil
		}

		msg, err := resolver.ExchangeContext(ctx, r)
		if err != nil {
			log.Debugln("[DNS Server] Exchange %s failed: %v", q.String(), err)
			return msg, err
		}
		msg.SetRcode(r, msg.Rcode)
		msg.Authoritative = true

		log.Debugln("[DNS] %s --> %s", msgToDomain(r), msgToIP(msg))
		return msg, nil
	}
}

func compose(middlewares []middleware, endpoint handler) handler {
	length := len(middlewares)
	h := endpoint
	for i := length - 1; i >= 0; i-- {
		middleware := middlewares[i]
		h = middleware(h)
	}

	return h
}

func NewHandler(resolver *Resolver, mapper *ResolverEnhancer) handler {
	middlewares := []middleware{}

	if resolver.hosts != nil {
		middlewares = append(middlewares, withHosts(resolver.hosts, mapper.mapping))
	}

	if mapper.mode == C.DNSFakeIP {
		middlewares = append(middlewares, withFakeIP(mapper.fakePool))
	}

	if mapper.mode != C.DNSNormal {
		middlewares = append(middlewares, withMapping(mapper.mapping))
	}

	return compose(middlewares, withResolver(resolver))
}
