package main

import (
	"container/ring"
	"crypto/aes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/net/ipv4"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Channel struct {
	lastRTPSeq  uint16
	firstPkt    bool
	pmtPid      uint16
	pmtPidFound bool
	ecmPid      uint16
	ecmPidFound bool
	masterKey   string
	aesKey1     []byte
	aesKey2     []byte
	mu          sync.Mutex
	buf         *ring.Ring
	c           *sync.Cond
	done        chan bool
	ioerr       bool
	numClients  int
	http        bool
}

const RingSize = 64

var runningChannelsMu sync.Mutex
var runningChannels map[string]*Channel

var ifi *net.Interface
var httpAddr string

type ChannelInfo struct {
	addr      string
	masterKey string
}

// channel name => ChannelInfo
var channels map[string]ChannelInfo

func newChannel(masterKey string, http bool) *Channel {
	ch := Channel{firstPkt: true, masterKey: masterKey, numClients: 1, http: http}
	if http {
		ch.buf = ring.New(RingSize)
		ch.c = sync.NewCond(&ch.mu)
		ch.done = make(chan bool)
		ch.http = true
	}
	return &ch
}

func (ch *Channel) parseRTP(pkt []byte) (int, error) {
	version := pkt[0] >> 6
	if version != 2 {
		return 0, fmt.Errorf("Unexpected RTP version %v", version)
	}
	hasExtension := (pkt[0] >> 4) & 1
	seq := binary.BigEndian.Uint16(pkt[2:4])
	if ch.firstPkt {
		ch.lastRTPSeq = seq - 1
		ch.firstPkt = false
	}
	if ch.lastRTPSeq+1 != seq {
		log.Println("RTP discontinuity detected")
	}
	ch.lastRTPSeq = seq
	extSize := 0
	if hasExtension > 0 {
		extSize = 4 + int(binary.BigEndian.Uint16(pkt[14:16])*4)
	}
	return 12 + extSize, nil
}

func (ch *Channel) processECM(pkt []byte) error {
	key, _ := hex.DecodeString(ch.masterKey)
	cipher, _ := aes.NewCipher([]byte(key))
	ecm := make([]byte, 64)
	for i := 0; i < 4; i++ {
		cipher.Decrypt(ecm[i*16:], pkt[29+i*16:])
	}
	if ecm[0] != 0x43 || ecm[1] != 0x45 || ecm[2] != 0x42 {
		return errors.New("Error decrypting ECM")
	}
	if pkt[5] == 0x81 {
		ch.aesKey1 = ecm[9 : 9+16]
		ch.aesKey2 = ecm[25 : 25+16]
	} else {
		ch.aesKey2 = ecm[9 : 9+16]
		ch.aesKey1 = ecm[25 : 25+16]
	}
	return nil
}

func (ch *Channel) decryptPacket(pkt []byte) {
	if ch.aesKey1 == nil || ch.aesKey2 == nil {
		return
	}
	scramble := (pkt[3] >> 6) & 3
	if scramble < 2 {
		return
	}
	var aesKey []byte
	if scramble == 2 {
		aesKey = ch.aesKey2
	} else if scramble == 3 {
		aesKey = ch.aesKey1
	}
	cipher, _ := aes.NewCipher([]byte(aesKey))
	pkt = pkt[4:]
	for len(pkt) > 16 {
		cipher.Decrypt(pkt, pkt)
		pkt = pkt[16:]
	}
}

func savePacket(pkt []byte) {
	f, err := os.OpenFile("dump.ts", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.Write(pkt); err != nil {
		log.Fatal(err)
	}
}

func (ch *Channel) parseEcmPid(desc []byte) error {
	//log.Printf("% x\n", desc)
	for len(desc) > 0 {
		tag := desc[0]
		length := desc[1]
		if tag == 0x09 {
			caid := binary.BigEndian.Uint16(desc[2:4])
			if caid == 0x5601 {
				ch.ecmPid = binary.BigEndian.Uint16(desc[4:6])
				ch.ecmPidFound = true
				//log.Printf("ECM pid=0x%x", ch.ecmPid)
				return nil
			}
		}
		desc = desc[2+length:]
	}
	return errors.New("Cannot find ECM PID")
}

func (ch *Channel) processPacket(pkt []byte) error {
	if pkt[0] != 0x47 {
		return fmt.Errorf("Expected sync byte but got: %v", pkt[0])
	}
	pid := binary.BigEndian.Uint16(pkt[1:3]) & 0x1fff
	if !ch.pmtPidFound && pid == 0 {
		// process PAT
		if pkt[4] != 0 {
			return errors.New("[PAT] Pointer fields are not supported yet")
		}
		if pkt[5] != 0 {
			return fmt.Errorf("Unexpected PAT table ID: %v", pkt[5])
		}
		ch.pmtPid = binary.BigEndian.Uint16(pkt[15:17]) & 0x1fff
		ch.pmtPidFound = true
		//log.Printf("PMT pid=0x%x", ch.pmtPid)
	}
	if !ch.ecmPidFound && ch.pmtPidFound && pid == ch.pmtPid {
		// process PMT
		if pkt[4] != 0 {
			return errors.New("[PMT] Pointer fields are not supported yet")
		}
		if pkt[5] != 2 {
			return fmt.Errorf("Unexpected PMT table ID: %v", pkt[5])
		}
		piLength := binary.BigEndian.Uint16(pkt[15:17]) & 0x03ff
		if err := ch.parseEcmPid(pkt[17 : 17+piLength]); err != nil {
			return err
		}
	}
	if ch.ecmPidFound && pid == ch.ecmPid {
		if err := ch.processECM(pkt); err != nil {
			return err
		}
	}
	ch.decryptPacket(pkt)
	if ch.http {
		ch.addToBuf(pkt)
	}
	return nil
	//savePacket(pkt)
	//log.Printf("% x\n", pkt)
}

func (ch *Channel) processRTP(payload []byte, offset int) error {
	if (len(payload)-offset)%188 != 0 {
		return fmt.Errorf("Unexpected RTP payload length: %v", len(payload))
	}
	pkt := payload[offset:]
	for len(pkt) > 0 {
		if err := ch.processPacket(pkt[:188]); err != nil {
			return err
		}
		pkt = pkt[188:]
	}
	return nil
}

func (ch *Channel) addToBuf(val interface{}) {
	ch.mu.Lock()
	ch.buf.Value = val
	ch.buf = ch.buf.Next()
	ch.c.Broadcast()
	ch.mu.Unlock()
}

func (ch *Channel) currentPtr() *ring.Ring {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.buf
}

func (ch *Channel) nextPtr(ptr *ring.Ring) (*ring.Ring, interface{}) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	for ptr == ch.buf && !ch.ioerr {
		ch.c.Wait()
	}
	if !ch.ioerr {
		return ptr.Next(), ptr.Value
	} else {
		return ptr, nil
	}
}

func (ch *Channel) closeBuf() {
	ch.mu.Lock()
	ch.ioerr = true
	ch.c.Broadcast()
	ch.mu.Unlock()
}

func decryptHTTP(ch *Channel, hostPort string) {
	host, _, _ := net.SplitHostPort(hostPort)
	group := net.ParseIP(host)
	c, err := net.ListenPacket("udp4", hostPort)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	p := ipv4.NewPacketConn(c)
	if err := p.JoinGroup(ifi, &net.UDPAddr{IP: group}); err != nil {
		log.Println(err)
		goto ioerr
	}
	defer p.LeaveGroup(ifi, &net.UDPAddr{IP: group})

	log.Println("Start decrypting channel @", hostPort)
	for {
		select {
		case <-ch.done:
			goto noclients
		default:
			// do nothing
		}
		pkt := make([]byte, 1500)
		p.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, _, err := p.ReadFrom(pkt)
		if err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
		payload := pkt[:n]
		offset, err := ch.parseRTP(payload)
		if err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
		if err := ch.processRTP(payload, offset); err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
	}
noclients:
	log.Println("No more clients, stop decrypting channel @", hostPort)
	ch.done <- true
	log.Println("Done @", hostPort)
	return

ioerr:
	log.Println("I/O error, stop decrypting channel @", hostPort)
	ch.closeBuf()
	<-ch.done
	ch.done <- true
	log.Println("Done @", hostPort)
}

func decryptRTP(ch *Channel, hostPort string, dest net.Conn) {
	host, _, _ := net.SplitHostPort(hostPort)
	group := net.ParseIP(host)
	c, err := net.ListenPacket("udp4", hostPort)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	p := ipv4.NewPacketConn(c)
	if err := p.JoinGroup(ifi, &net.UDPAddr{IP: group}); err != nil {
		log.Println(err)
		goto ioerr
	}
	defer p.LeaveGroup(ifi, &net.UDPAddr{IP: group})

	log.Println("Start decrypting channel @", hostPort)
	for {
		pkt := make([]byte, 1500)
		p.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, _, err := p.ReadFrom(pkt)
		if err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
		payload := pkt[:n]
		offset, err := ch.parseRTP(payload)
		if err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
		if err := ch.processRTP(payload, offset); err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
		if _, err := dest.Write(payload); err != nil {
			log.Printf("%v @ %v", err, hostPort)
			goto ioerr
		}
	}

ioerr:
	log.Println("I/O error, stop decrypting channel @", hostPort)
	log.Println("Done @", hostPort)
}

func rtpHandler(w http.ResponseWriter, req *http.Request) {
	// requestURI should be /rtp/CNN/192.168.1.1:51820
	parts := strings.Split(req.RequestURI[5:], "/")
	if len(parts) != 2 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	chName := parts[0]
	chInfo, ok := channels[chName];
	if !ok {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	addr := parts[1]
	if _, _, err := net.SplitHostPort(addr); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	dest, err := net.Dial("udp", addr)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ch := newChannel(chInfo.masterKey, false)
	go decryptRTP(ch, chInfo.addr, dest)
}

func chHandler(w http.ResponseWriter, req *http.Request) {
	chName := req.RequestURI[4:]
	chInfo, ok := channels[chName]
	if !ok {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	runningChannelsMu.Lock()
	ch, ok := runningChannels[chInfo.addr]
	if !ok {
		ch = newChannel(chInfo.masterKey, true)
		runningChannels[chInfo.addr] = ch
		go decryptHTTP(ch, chInfo.addr)
	} else {
		ch.numClients += 1
	}
	runningChannelsMu.Unlock()

	log.Println("Start serving client", req.RemoteAddr)
	ptr := ch.currentPtr()
	var val interface{}
	for {
		ptr, val = ch.nextPtr(ptr)
		if val == nil {
			break
		}
		_, err := w.Write(val.([]byte))
		if err != nil {
			break
		}
	}

	log.Println("Stop serving client", req.RemoteAddr)
	runningChannelsMu.Lock()
	if ch, ok = runningChannels[chInfo.addr]; ok {
		ch.numClients -= 1
		if ch.numClients == 0 {
			ch.done <- true
			<-ch.done
			delete(runningChannels, chInfo.addr)
		}
	}
	runningChannelsMu.Unlock()
}

func m3uHandler(w http.ResponseWriter, req *http.Request) {
	io.WriteString(w, "#EXTM3U\n")
	keys := make([]string, 0)
	for k, _ := range channels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		chName, _ := url.PathUnescape(k)
		fmt.Fprintf(w, "#EXTINF:-1, %s\n", chName)
		fmt.Fprintf(w, "http://%s/ch/%s\n", httpAddr, k)
	}
}

func fetchChannels(chURL string) {
	resp, err := http.Get(chURL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	var f interface{}
	err = json.Unmarshal(body, &f)
	if err != nil {
		log.Fatal(err)
	}
	m := f.(map[string]interface{})
	chdate := m["date"]
	all := m["channels"].([]interface{})
	for _, c := range all {
		v := c.([]interface{})
		name := v[0].(string)
		addr := v[1].(string)
		switch key := v[2].(type) {
		case string:
			name = url.PathEscape(name)
			// strip "igmp://" from address
			channels[name] = ChannelInfo{addr[7:], key}
		case float64:
			// ignore
		}
	}
	log.Printf("%d channels loaded, last updated on %s\n", len(channels), chdate)
}

func main() {
	ifname := flag.String("i", "eth0", "Multicast interface")
	chURL := flag.String("c", "", "Channels file URL")
	flag.StringVar(&httpAddr, "a", "localhost:8080", "Network address (host:port) for the HTTP server")
	flag.Parse()
	var err error
	ifi, err = net.InterfaceByName(*ifname)
	if err != nil {
		fmt.Printf("No such network interface: %s\n", *ifname)
		os.Exit(1)
	}
	channels = make(map[string]ChannelInfo)
	if *chURL != "" {
		ticker := time.NewTicker(1 * time.Hour)
		go func() {
			for {
				fetchChannels(*chURL)
				<-ticker.C
			}
		}()
	}

	log.Printf("Starting HTTP server on %s, multicast interface: %s\n", httpAddr, *ifname)
	runningChannels = make(map[string]*Channel)
	http.HandleFunc("/rtp/", rtpHandler)
	http.HandleFunc("/ch/", chHandler)
	http.HandleFunc("/channels.m3u", m3uHandler)
	log.Fatal(http.ListenAndServe(httpAddr, nil))
}
