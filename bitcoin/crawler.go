package bitcoin

import (
	"context"
	"encoding/json"
	"net"

	// "errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/cenkalti/backoff/v4"

	// "github.com/ltcsuite/ltcd/wire"

	// "github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/libp2p/go-libp2p/core/peer"
	// "github.com/libp2p/go-libp2p/core/peerstore"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	log "github.com/sirupsen/logrus"

	// "go.uber.org/atomic"

	"github.com/dennis-tra/nebula-crawler/config"
	"github.com/dennis-tra/nebula-crawler/core"
	"github.com/dennis-tra/nebula-crawler/db"
	"github.com/dennis-tra/nebula-crawler/db/models"
)

const MaxCrawlRetriesAfterTimeout = 2 // magic

type CrawlerConfig struct {
	DialTimeout  time.Duration
	AddrDialType config.AddrType
	KeepENR      bool
	LogErrors    bool
}

type Crawler struct {
	id           string
	cfg          *CrawlerConfig
	conn         net.Conn
	crawledPeers int
	done         chan struct{}
}

var _ core.Worker[PeerInfo, core.CrawlResult[PeerInfo]] = (*Crawler)(nil)

func (c *Crawler) Work(ctx context.Context, task PeerInfo) (core.CrawlResult[PeerInfo], error) {
	logEntry := log.WithFields(log.Fields{
		"crawlerID":  c.id,
		"remoteID":   task.ID().ShortString(),
		"crawlCount": c.crawledPeers,
	})
	println("Crawling peer")
	defer logEntry.Debugln("Crawled peer")

	crawlStart := time.Now()

	// start crawling both ways
	bitcoinResultCh := c.crawlBitcoin(ctx, task)
	// discV5ResultCh := c.crawlDiscV5(ctx, task)

	libp2pResult := <-bitcoinResultCh
	// discV5Result := <-discV5ResultCh

	properties := c.PeerProperties(&task.AddrInfo)

	if libp2pResult.Transport != "" {
		properties["transport"] = libp2pResult.Transport
	}

	if libp2pResult.ConnClosedImmediately {
		properties["direct_close"] = true
	}

	if libp2pResult.GenTCPAddr {
		properties["gen_tcp_addr"] = true
	}

	// keep track of all unknown connection errors
	if libp2pResult.ConnectErrorStr == models.NetErrorUnknown && libp2pResult.ConnectError != nil {
		properties["connect_error"] = libp2pResult.ConnectError.Error()
	}

	// keep track of all unknown crawl errors
	if libp2pResult.ErrorStr == models.NetErrorUnknown && libp2pResult.Error != nil {
		properties["crawl_error"] = libp2pResult.Error.Error()
	}

	data, err := json.Marshal(properties)
	if err != nil {
		log.WithError(err).WithField("properties", properties).Warnln("Could not marshal peer properties")
	}

	if len(libp2pResult.ListenAddrs) > 0 {
		task.AddrInfo.Addr = libp2pResult.ListenAddrs
	}

	cr := core.CrawlResult[PeerInfo]{
		CrawlerID:           c.id,
		Info:                task,
		CrawlStartTime:      crawlStart,
		RoutingTableFromAPI: false,
		RoutingTable:        libp2pResult.RoutingTable,
		Agent:               libp2pResult.Agent,
		Protocols:           libp2pResult.Protocols,
		ConnectError:        libp2pResult.ConnectError,
		ConnectErrorStr:     libp2pResult.ConnectErrorStr,
		CrawlError:          libp2pResult.Error,
		CrawlErrorStr:       libp2pResult.ErrorStr,
		CrawlEndTime:        time.Now(),
		ConnectStartTime:    libp2pResult.ConnectStartTime,
		ConnectEndTime:      libp2pResult.ConnectEndTime,
		Properties:          data,
		LogErrors:           c.cfg.LogErrors,
	}

	// We've now crawled this peer, so increment
	c.crawledPeers++

	return cr, nil
}

func (c *Crawler) PeerProperties(node *AddrInfo) map[string]any {
	// TODO: to be implemented later
	properties := map[string]any{}

	properties["NA"] = true

	return properties
}

type BitcoinResult struct {
	ConnectStartTime      time.Time
	ConnectEndTime        time.Time
	ConnectError          error
	ConnectErrorStr       string
	Agent                 string
	Protocols             []string
	ListenAddrs           []ma.Multiaddr
	Transport             string // the transport of a successful connection
	ConnClosedImmediately bool   // whether conn was no error but still unconnected
	GenTCPAddr            bool   // whether a TCP address was generated
	Error                 error
	ErrorStr              string
	RoutingTable          *core.RoutingTable[PeerInfo]
}

func (c *Crawler) crawlBitcoin(ctx context.Context, pi PeerInfo) chan BitcoinResult {
	resultCh := make(chan BitcoinResult)

	go func() {
		result := BitcoinResult{}

		// sanitize the given addresses like removing UDP-only addresses and
		// adding corresponding TCP addresses.
		sanitizedAddrs, generated := sanitizeAddrs(pi.Addrs())

		// keep track if we generated a TCP address to dial
		result.GenTCPAddr = generated

		addrInfo := peer.AddrInfo{
			ID:    pi.ID(),
			Addrs: sanitizedAddrs,
		}

		var conn net.Conn
		result.ConnectStartTime = time.Now()
		conn, result.ConnectError = c.connect(ctx, addrInfo) // use filtered addr list
		c.conn = conn
		result.ConnectEndTime = time.Now()

		// If we could successfully connect to the peer we actually crawl it.
		if result.ConnectError == nil {

			// keep track of the transport of the open connection
			result.Transport = "tcp"

			// wait for the Identify exchange to complete (no-op if already done)
			// the internal timeout is set to 30 s. When crawling we only allow 5s.
			// timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// defer cancel()

			// select {
			// case <-timeoutCtx.Done():
			// 	// identification timed out.
			// case <-c.host.IDService().IdentifyWait(conn):
			// 	// identification may have succeeded.
			// }

			// Extract information from peer store
			// ps := conn.

			// // Extract agent
			// if agent, err := ps.Get(pi.ID(), "AgentVersion"); err == nil {
			// 	result.Agent = agent.(string)
			// }

			// // Extract protocols
			// if protocols, err := ps.GetProtocols(pi.ID()); err == nil {
			// 	result.Protocols = make([]string, len(protocols))
			// 	for i := range protocols {
			// 		result.Protocols[i] = string(protocols[i])
			// 	}
			// }

			// Extract listen addresses
			// result.ListenAddrs = ps.Addrs(pi.ID())
		} else {
			result.Error = result.ConnectError
		}

		result.RoutingTable = &core.RoutingTable[PeerInfo]{
			PeerID:    pi.ID(),
			Neighbors: []PeerInfo{},
			ErrorBits: uint16(0), // FIXME
			Error:     result.Error,
		}

		// if there was a connection error, parse it to a known one
		if result.ConnectError != nil {
			result.ConnectErrorStr = db.NetError(result.ConnectError)
		} else {
			// Free connection resources
			if err := c.conn.Close(); err != nil {
				log.WithError(err).WithField("remoteID", pi.ID().ShortString()).Warnln("Could not close connection to peer")
			}
		}

		if result.Error != nil {
			result.ErrorStr = db.NetError(result.Error)
		}

		// send the result back and close channel
		select {
		case resultCh <- result:
		case <-ctx.Done():
		}

		close(resultCh)
	}()

	return resultCh
}

type BitcoinNodeResult struct {
	ProtocolVersion int32
	UserAgent       string
	pver            uint32
}

func (c *Crawler) Handshake() (BitcoinNodeResult, error) {
	result := BitcoinNodeResult{}
	if c.conn == nil {
		return result, fmt.Errorf("Peer is not connected, can't handshake.")
	}

	log.WithField("Address", c.conn.RemoteAddr()).Debug("[%s] Starting handshake.")

	nonce, err := wire.RandomUint64()
	if err != nil {
		return result, err
	}

	localAddr := &wire.NetAddress{
		IP:   c.conn.LocalAddr().(*net.TCPAddr).IP,
		Port: uint16(c.conn.LocalAddr().(*net.TCPAddr).Port),
	}
	remoteAddr := &wire.NetAddress{
		IP:   c.conn.RemoteAddr().(*net.TCPAddr).IP,
		Port: uint16(c.conn.RemoteAddr().(*net.TCPAddr).Port),
	}

	msgVersion := wire.NewMsgVersion(localAddr, remoteAddr, nonce, 0)

	// msgVersion := wire.NewMsgVersion(p.conn.LocalAddr(), p.conn.RemoteAddr(), p.nonce, 0)
	msgVersion.UserAgent = "nebula/"
	msgVersion.DisableRelayTx = true
	if err := c.WriteMessage(msgVersion); err != nil {
		return result, err
	}

	// Read the response version.
	msg, _, err := c.ReadMessage()
	if err != nil {
		return result, err
	}
	vmsg, ok := msg.(*wire.MsgVersion)
	if !ok {
		return result, fmt.Errorf("Did not receive version message: %T", vmsg)
	}

	result.ProtocolVersion = vmsg.ProtocolVersion
	result.UserAgent = vmsg.UserAgent

	// Negotiate protocol version.
	if uint32(vmsg.ProtocolVersion) < wire.ProtocolVersion {
		result.pver = uint32(vmsg.ProtocolVersion)
	}
	log.Debugf("[%s] -> Version: %s", c.conn.RemoteAddr(), vmsg.UserAgent)

	// Normally we'd check if vmsg.Nonce == p.nonce but the crawler does not
	// accept external connections so we skip it.

	// Send verack.
	if err := c.WriteMessage(wire.NewMsgVerAck()); err != nil {
		return result, err
	}

	return result, nil
}

func (c *Crawler) WriteMessage(msg wire.Message) error {
	return wire.WriteMessage(c.conn, msg, wire.ProtocolVersion, wire.MainNet)
}

func (c *Crawler) ReadMessage() (wire.Message, []byte, error) {
	return wire.ReadMessage(c.conn, wire.ProtocolVersion, wire.MainNet)
}

// connect establishes a connection to the given peer. It also handles metric capturing.
func (c *Crawler) connect(ctx context.Context, pi peer.AddrInfo) (net.Conn, error) {
	if len(pi.Addrs) == 0 {
		return nil, fmt.Errorf("skipping node as it has no public IP address") // change knownErrs map if changing this msg
	}
	println("===========connect=============")
	println("===============================")
	println("===============================")
	println("===============================")
	// init an exponential backoff
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = time.Second
	bo.MaxInterval = 10 * time.Second
	bo.MaxElapsedTime = time.Minute

	var retry int = 0

	for {
		logEntry := log.WithFields(log.Fields{
			"timeout":  c.cfg.DialTimeout.String(),
			"remoteID": pi.ID.String(),
			"retry":    retry,
			"maddrs":   pi.Addrs,
		})
		logEntry.Debugln("Connecting to peer", pi.ID.ShortString())

		// save addresses into the peer store temporarily

		// conn, err := c.host.Network().DialPeer(timeoutCtx, pi.ID)
		netAddr, err := manet.ToNetAddr(pi.Addrs[0])

		pi.Addrs[0].ValueForProtocol(ma.P_IP4)
		conn, err := net.DialTimeout(netAddr.Network(), netAddr.String(), c.cfg.DialTimeout)

		if err == nil {
			return conn, nil
		}

		switch {
		case strings.Contains(err.Error(), db.ErrorStr[models.NetErrorConnectionRefused]):
			// Might be transient because the remote doesn't want us to connect. Try again!
		case strings.Contains(err.Error(), db.ErrorStr[models.NetErrorConnectionGated]):
			// Hints at a configuration issue and should not happen, but if it
			// does it could be transient. Try again anyway, but at least log a warning.
			logEntry.WithError(err).Warnln("Connection gated!")
		case strings.Contains(err.Error(), db.ErrorStr[models.NetErrorCantAssignRequestedAddress]):
			// Transient error due to local UDP issues. Try again!
		case strings.Contains(err.Error(), "dial backoff"):
			// should not happen because we disabled backoff checks with our
			// go-libp2p fork. Try again anyway, but at least log a warning.
			logEntry.WithError(err).Warnln("Dial backoff!")
		case strings.Contains(err.Error(), "RESOURCE_LIMIT_EXCEEDED (201)"): // thrown by a circuit relay
			// We already have too many open connections over a relay. Try again!
		default:
			logEntry.WithError(err).Debugln("Failed connecting to peer", pi.ID.ShortString())
			return nil, err
		}

		sleepDur := bo.NextBackOff()
		if sleepDur == backoff.Stop {
			logEntry.WithError(err).Debugln("Exceeded retries connecting to peer", pi.ID.ShortString())
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleepDur):
			retry += 1
			continue
		}

	}
}

// sanitizeAddrs takes the list of multi addresses and removes any UDP-only
// multi address because we cannot dial UDP only addresses anyway. However, if
// there is no other reliable transport address like TCP or QUIC we use the UDP
// IP address + port and craft a TCP address out of it. The UDP address will
// still be removed and replaced with TCP.
func sanitizeAddrs(maddrs []ma.Multiaddr) ([]ma.Multiaddr, bool) {
	newMaddrs := make([]ma.Multiaddr, 0, len(maddrs))
	for _, maddr := range maddrs {
		if _, err := maddr.ValueForProtocol(ma.P_TCP); err == nil {
			newMaddrs = append(newMaddrs, maddr)
		} else if _, err := maddr.ValueForProtocol(ma.P_UDP); err == nil {
			_, quicErr := maddr.ValueForProtocol(ma.P_QUIC)
			_, quicV1Err := maddr.ValueForProtocol(ma.P_QUIC_V1)
			if quicErr == nil || quicV1Err == nil {
				newMaddrs = append(newMaddrs, maddr)
			}
		}
	}

	if len(newMaddrs) > 0 {
		return newMaddrs, false
	}

	for i, maddr := range maddrs {
		udp, err := maddr.ValueForProtocol(ma.P_UDP)
		if err != nil {
			continue
		}

		ip := ""
		ip4, err := maddr.ValueForProtocol(ma.P_IP4)
		if err != nil {
			ip6, err := maddr.ValueForProtocol(ma.P_IP6)
			if err != nil {
				continue
			}
			ip = "/ip6/" + ip6
		} else {
			ip = "/ip4/" + ip4
		}

		tcpMaddr, err := ma.NewMultiaddr(ip + "/tcp/" + udp)
		if err != nil {
			continue
		}

		for _, remaining := range maddrs[i+1:] {
			newMaddrs = append(newMaddrs, remaining)
		}

		newMaddrs = append(newMaddrs, tcpMaddr)

		return newMaddrs, true
	}

	return maddrs, false
}

// func (c *Crawler) crawlDiscV5(ctx context.Context, pi PeerInfo) chan DiscV5Result {
// 	resultCh := make(chan DiscV5Result)

// 	go func() {
// 		result := DiscV5Result{}

// 		// all neighbors of pi. We're using a map to deduplicate.
// 		allNeighbors := map[string]PeerInfo{}

// 		// errorBits tracks at which CPL errors have occurred.
// 		// 0000 0000 0000 0000 - No error
// 		// 0000 0000 0000 0001 - An error has occurred at CPL 0
// 		// 1000 0000 0000 0001 - An error has occurred at CPL 0 and 15
// 		errorBits := atomic.NewUint32(0)

// 		timeouts := 0
// 		enr, err := c.listener.RequestENR(pi.Node)
// 		if err != nil {
// 			timeouts += 1
// 			result.ENR = pi.Node
// 		} else {
// 			result.ENR = enr
// 			now := time.Now()
// 			result.RespondedAt = &now
// 		}

// 		// loop through the buckets sequentially because discv5 is also doing that
// 		// internally, so we won't gain much by spawning multiple parallel go
// 		// routines here. Stop the process as soon as we have received a timeout and
// 		// don't let the following calls time out as well.
// 		for i := 0; i <= discover.NBuckets; i++ { // 17 is maximum
// 			var neighbors []*enode.Node
// 			neighbors, err = c.listener.FindNode(pi.Node, []uint{uint(discover.HashBits - i)})
// 			if err != nil {

// 				if errors.Is(err, discover.ErrTimeout) {
// 					timeouts += 1
// 					if timeouts < MaxCrawlRetriesAfterTimeout {
// 						continue
// 					}
// 				}

// 				errorBits.Add(1 << i)
// 				err = fmt.Errorf("getting closest peer with CPL %d: %w", i, err)
// 				break
// 			}
// 			timeouts = 0

// 			if result.RespondedAt == nil {
// 				now := time.Now()
// 				result.RespondedAt = &now
// 			}

// 			for _, n := range neighbors {
// 				npi, err := NewPeerInfo(n)
// 				if err != nil {
// 					log.WithError(err).Warnln("Failed parsing ethereum node neighbor")
// 					continue
// 				}
// 				allNeighbors[string(npi.peerID)] = npi
// 			}
// 		}

// 		result.DoneAt = time.Now()
// 		// if we have at least a successful result, don't record error
// 		if noSuccessfulRequest(err, errorBits.Load()) {
// 			result.Error = err
// 		}

// 		result.RoutingTable = &core.RoutingTable[PeerInfo]{
// 			PeerID:    pi.ID(),
// 			Neighbors: []PeerInfo{},
// 			ErrorBits: uint16(errorBits.Load()),
// 			Error:     result.Error,
// 		}

// 		for _, n := range allNeighbors {
// 			result.RoutingTable.Neighbors = append(result.RoutingTable.Neighbors, n)
// 		}

// 		// if there was a connection error, parse it to a known one
// 		if result.Error != nil {
// 			result.ErrorStr = db.NetError(result.Error)
// 		}

// 		// send the result back and close channel
// 		select {
// 		case resultCh <- result:
// 		case <-ctx.Done():
// 		}
// 		close(resultCh)
// 	}()

// 	return resultCh
// }

// noSuccessfulRequest returns true if the given error is non nil, and all bits
// of the given errorBits are set. This means that no successful request has
// been made. This is equivalent to verifying that all righmost bits are equal
// to 1, or that the errorBits is a power of 2 minus 1.
//
// Examples:
// 0b00000011 -> true
// 0b00000111 -> true
// 0b00001101 -> false
func noSuccessfulRequest(err error, errorBits uint32) bool {
	return err != nil && errorBits&(errorBits+1) == 0
}
