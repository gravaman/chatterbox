package main

import (
	// "bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/marcusolsson/tui-go"
)

const (
	DEFAULT_PORT = 8000
)

var history *tui.Box

// Opts encapsulates cli options.
type Opts struct {
	debug *bool
	help  *bool
	port  *string
	name  *string
	peer  *string
}

var opts = Opts{
	debug: flag.Bool("d", false, "debug"),
	help:  flag.Bool("h", false, "help"),
	port:  flag.String("p", strconv.Itoa(DEFAULT_PORT), "port"),
	name:  flag.String("n", "anon", "display name"),
	peer:  flag.String("j", "", "other peer to join"),
}

// Stage used to describe program lifecycle in debugging.
type Stage int

const (
	Launch Stage = 1 + iota
	EventLoop
	NetListener
	UserListener
	Exit
)

func printStage(s Stage) {
	switch s {
	case Launch:
		fmt.Printf("[+] Launching ChatterBox\n")
	case EventLoop:
		fmt.Printf("[+] Event loop launched\n")
	case NetListener:
		fmt.Printf("[+] Listening for peers at %s:%s\n", localIP4(), servicePort())
	case UserListener:
		fmt.Printf("[+] Listening to stdin for user input\n")
	case Exit:
		fmt.Printf("[+] Shutting Down ChatterBox\n")
	default:
		log.Fatal("Unknown stage")
	}
}

// Msg represents a message for a chat.
type Msg struct {
	Txt  string
	User Peer
	Time time.Time
}

// Peer represents a chat user.
type Peer struct {
	Name string
	Addr string
}

// P2PNet represents the p2p network.
type P2PNet struct {
	Self            Peer
	Peers           Peers
	rcvMsg          chan Msg
	peerJoin        chan Peer
	peerLeft        chan Peer
	peersCurrent    chan Peers
	getCurrentPeers chan bool
	userMsg         chan Msg
}

// Start launches the event loop, user listener, and net listener.
// A peer join event is optionally triggered depending on the cli input.
func (n *P2PNet) Start(sbv *sidebarView, ui tui.UI) {
	go n.eventLoop(sbv, ui)
	go n.netListener()
	if *opts.peer != "" {
		n.peerJoin <- Peer{Addr: *opts.peer}
	}
}

// eventLoop handles user and network io events.
func (n *P2PNet) eventLoop(sbv *sidebarView, ui tui.UI) {
	if *opts.debug {
		printStage(EventLoop)
	}
	for {
		select {
		case peer := <-n.peerJoin:
			if !n.knownPeer(peer) {
				go n.sendJoin(sbv, peer)
			}
		case <-n.getCurrentPeers:
			n.peersCurrent <- n.Peers
		case peer := <-n.peerLeft:
			delete(n.Peers, peer.Addr)
			history.Append(tui.NewHBox(
				tui.NewLabel(fmt.Sprintf("# <%s> <%s> (%s) has left the chat", peer.Name, peer.Addr)),
				tui.NewSpacer(),
			))
			sbv.removePeep(peer)
		case m := <-n.rcvMsg:
			history.Append(tui.NewHBox(
				tui.NewLabel(m.Time.Format("15:04")),
				tui.NewPadder(1, 0, tui.NewLabel(fmt.Sprintf("<%s>", m.User.Name))),
				tui.NewLabel(m.Txt),
				tui.NewSpacer(),
			))
			ui.Repaint()
		case m := <-n.userMsg:
			for _, peer := range n.Peers {
				go n.sendChat(peer, m)
			}
		}
	}
}

type JoinRes struct {
	*Peer
	Others Peers
}

// sendJoin sends a join request to the given peer. If the req errors
// the peer is assumed to have left. Each peer included in a successful
// response is directed to the event loop for subsequent join requests.
func (n *P2PNet) sendJoin(sbv *sidebarView, peer Peer) {
	URL := "http://" + peer.Addr + "/join"
	var b *bytes.Buffer
	if qs, err := json.Marshal(n.Self); err != nil {
		log.Fatal(err)
	} else {
		b = bytes.NewBuffer(qs)
	}

	res, err := http.Post(URL, "application/json", b)
	if err != nil {
		n.peerLeft <- peer
		return
	}
	defer res.Body.Close()

	joinRes := new(JoinRes)
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(&joinRes); err != nil {
		log.Fatal(err)
	}
	if peer.Name == "" {
		peer.Name = joinRes.Name
	}

	if peer.Name != n.Self.Name {
		sbv.addPeep(peer)
		history.Append(tui.NewHBox(
			tui.NewLabel(fmt.Sprintf("# <%s> (%s) has joined the chat", peer.Name, peer.Addr)),
			tui.NewSpacer(),
		))
		n.Peers[peer.Addr] = peer
		n.peerJoin <- peer
	}

	for _, other := range joinRes.Others {
		n.peerJoin <- other
	}
}

// sendChat sends a chat message to a given peer. If the req errors
// the peer is assumed to have left.
func (n *P2PNet) sendChat(peer Peer, m Msg) {
	URL := "http://" + peer.Addr + "/chat"
	var b *bytes.Buffer
	if qs, err := json.Marshal(m); err != nil {
		log.Fatal(err)
	} else {
		b = bytes.NewBuffer(qs)
	}

	if _, err := http.Post(URL, "application/json", b); err != nil {
		n.peerLeft <- peer
	}
}

// knownPeer checks whether given peer is in current peers list.
func (n *P2PNet) knownPeer(peer Peer) bool {
	if peer.Addr == n.Self.Addr {
		return true
	}
	_, ok := n.Peers[peer.Addr]
	return ok
}

// netListener reads network input and sends to event loop.
func (n *P2PNet) netListener() {
	http.HandleFunc("/chat", chatHandler(n))
	http.HandleFunc("/join", joinHandler(n))

	if *opts.debug {
		printStage(NetListener)
	}

	log.Fatal(http.ListenAndServe(n.Self.Addr, nil))
}

// chatHandler reads chat messages and sends to event loop.
func chatHandler(n *P2PNet) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		msg := new(Msg)
		dec := json.NewDecoder(r.Body)
		err := dec.Decode(msg)
		if err != nil {
			fmt.Errorf("Error unmarshalling chat msg from: %v", err)
		}
		n.rcvMsg <- *msg
	}
}

// joinHandler reads join reqs and directs to event loop. The current
// peer list is sent back as a response.
func joinHandler(n *P2PNet) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		peer := new(Peer)
		dec := json.NewDecoder(r.Body)
		err := dec.Decode(peer)
		if err != nil {
			fmt.Errorf("Error unmarshalling join request from: %v", err)
		}
		if peer.Name == "" {
			fmt.Println("JOIN HANDLER PEER.NAME:", peer.Name)
			peer.Name = n.Self.Name
			fmt.Println("JOIN HANDLER SELF.NAME:", n.Self.Name)
		}
		n.peerJoin <- *peer
		n.getCurrentPeers <- true

		enc := json.NewEncoder(w)
		joinRes := JoinRes{
			Peer:   peer,
			Others: <-n.peersCurrent,
		}
		enc.Encode(joinRes)
	}
}

// userListener reads stdin user input and sends to event loop.
func (n *P2PNet) userListener(input *tui.Entry) {
	input.OnSubmit(func(entry *tui.Entry) {
		m := Msg{
			User: n.Self,
			Txt:  entry.Text(),
			Time: time.Now(),
		}
		history.Append(tui.NewHBox(
			tui.NewLabel(m.Time.Format("15:04")),
			tui.NewPadder(1, 0, tui.NewLabel(fmt.Sprintf("<%s>", m.User.Name))),
			tui.NewLabel(m.Txt),
			tui.NewSpacer(),
		))
		input.SetText("")
		n.userMsg <- m
	})
}

// NewP2PNet initializes a new p2p network with given seed peer.
func NewP2PNet(self Peer) *P2PNet {
	return &P2PNet{
		Self:            self,
		Peers:           make(Peers),
		peerJoin:        make(chan Peer),
		peerLeft:        make(chan Peer),
		peersCurrent:    make(chan Peers),
		getCurrentPeers: make(chan bool),
		userMsg:         make(chan Msg),
		rcvMsg:          make(chan Msg),
	}
}

// Peers is a map of current Peers with address for key.
type Peers map[string]Peer

func (ps *Peers) peerNames() []string {
	var pnames []string
	for _, peer := range map[string]Peer(*ps) {
		pnames = append(pnames, peer.Name)
	}
	return pnames
}

func localIP4() string {
	host, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		log.Fatal(err)
	}
	var ip4 net.IP
	ipOut := "localhost"
	for _, addr := range addrs {
		if ip4 = addr.To4(); ip4 != nil {
			ipOut = fmt.Sprintf("%s", ip4)
			break
		}
	}
	return ipOut
}

func servicePort() string {
	if *opts.port != "" {
		return *opts.port
	}
	return strconv.Itoa(DEFAULT_PORT)
}

type sidebarView struct {
	tLabel *tui.Label
	pLabel *tui.Label
	pnames []string

	*tui.Box
}

func (sbv *sidebarView) removePeep(peer Peer) {
	var found bool
	var idx int
	for i, name := range sbv.pnames {
		if name == peer.Name {
			found = true
			idx = i
			break
		}
	}
	if found {
		sbv.pnames = append(sbv.pnames[:idx], sbv.pnames[idx+1:]...)
		sbv.pLabel.SetText(strings.Join(sbv.pnames, "\n"))
	}
}

func (sbv *sidebarView) addPeep(peer Peer) {
	if peer.Name == "" {
		return
	}
	sbv.pnames = append(sbv.pnames, peer.Name)
	sbv.pLabel.SetText(strings.Join(sbv.pnames, "\n"))
}

func newSidebarView(title string, peers Peers) *sidebarView {
	pnames := peers.peerNames()
	pLabel := tui.NewLabel(strings.Join(pnames, "\n"))
	tLabel := tui.NewLabel(title)
	v := &sidebarView{
		tLabel: tLabel,
		pLabel: pLabel,
		pnames: pnames,
	}
	v.Box = tui.NewVBox(
		tLabel,
		pLabel,
		tui.NewSpacer(),
	)
	v.Box.SetBorder(true)
	return v
}

func main() {
	// handle cli opts
	flag.Parse()

	if *opts.debug {
		printStage(Launch)
		defer printStage(Exit)
	}

	if *opts.help {
		fmt.Printf("\nChatterBox Usage\n")
		flag.PrintDefaults()
		fmt.Println()
		os.Exit(0)
	}

	// network  setup
	me := Peer{*opts.name, localIP4() + ":" + servicePort()}
	n := NewP2PNet(me)

	// tui setup
	sbv := newSidebarView("PEEPS", n.Peers)

	history = tui.NewVBox()
	historyScroll := tui.NewScrollArea(history)
	historyScroll.SetAutoscrollToBottom(true)

	historyBox := tui.NewVBox(historyScroll)
	historyBox.SetBorder(true)

	input := tui.NewEntry()
	input.SetFocused(true)
	input.SetSizePolicy(tui.Expanding, tui.Maximum)

	// user input setup
	n.userListener(input)

	inputBox := tui.NewHBox(input)
	inputBox.SetBorder(true)
	inputBox.SetSizePolicy(tui.Expanding, tui.Maximum)

	// launch tui
	chat := tui.NewVBox(historyBox, inputBox)
	chat.SetSizePolicy(tui.Expanding, tui.Expanding)

	root := tui.NewHBox(sbv.Box, chat)

	ui, err := tui.New(root)
	if err != nil {
		log.Fatal(err)
	}

	ui.SetKeybinding("Esc", func() { ui.Quit() })

	// launch app
	n.Start(sbv, ui)

	// launch tui
	if err := ui.Run(); err != nil {
		log.Fatal(err)
	}
}
