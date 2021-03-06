package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/awalterschulze/gographviz"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
	"github.com/urfave/cli"
	"golang.org/x/net/context"
)

// TODO(roasbeef): cli logic for supporting both positional and unix style
// arguments.

func printJson(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	json.Indent(&out, b, "", "\t")
	out.WriteTo(os.Stdout)
}

func printRespJson(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
		EmitDefaults: true,
		Indent:       "    ",
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

var NewAddressCommand = cli.Command{
	Name:   "newaddress",
	Usage:  "generates a new address. Three address types are supported: p2wkh, np2wkh, p2pkh",
	Action: newAddress,
}

func newAddress(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	stringAddrType := ctx.Args().Get(0)

	// Map the string encoded address type, to the concrete typed address
	// type enum. An unrecognized address type will result in an error.
	var addrType lnrpc.NewAddressRequest_AddressType
	switch stringAddrType { // TODO(roasbeef): make them ints on the cli?
	case "p2wkh":
		addrType = lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH
	case "np2wkh":
		addrType = lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH
	case "p2pkh":
		addrType = lnrpc.NewAddressRequest_PUBKEY_HASH
	default:
		return fmt.Errorf("invalid address type %v, support address type "+
			"are: p2wkh, np2wkh, p2pkh", stringAddrType)
	}

	ctxb := context.Background()
	addr, err := client.NewAddress(ctxb, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	if err != nil {
		return err
	}

	printRespJson(addr)
	return nil
}

var SendCoinsCommand = cli.Command{
	Name:        "sendcoins",
	Description: "send a specified amount of bitcoin to the passed address",
	Usage:       "sendcoins --addr=<bitcoin addresss> --amt=<num coins in satoshis>",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "addr",
			Usage: "the bitcoin address to send coins to on-chain",
		},
		// TODO(roasbeef): switch to BTC on command line? int may not be sufficient
		cli.IntFlag{
			Name:  "amt",
			Usage: "the number of bitcoin denominated in satoshis to send",
		},
	},
	Action: sendCoins,
}

func sendCoins(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.SendCoinsRequest{
		Addr:   ctx.String("addr"),
		Amount: int64(ctx.Int("amt")),
	}
	txid, err := client.SendCoins(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(txid)
	return nil
}

var SendManyCommand = cli.Command{
	Name: "sendmany",
	Description: "create and broadcast a transaction paying the specified " +
		"amount(s) to the passed address(es)",
	Usage:  `sendmany '{"ExampleAddr": NumCoinsInSatoshis, "SecondAddr": NumCoins}'`,
	Action: sendMany,
}

func sendMany(ctx *cli.Context) error {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().Get(0)
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{amountToAddr})
	if err != nil {
		return err
	}

	printRespJson(txid)
	return nil
}

var ConnectCommand = cli.Command{
	Name:  "connect",
	Usage: "connect to a remote lnd peer: <pubkey>@host (--perm=true|false])",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "perm",
			Usage: "If true, then the daemon will attempt to persistently " +
				"connect to the target peer. If false then the call " +
				"will be synchronous.",
		},
	},
	Action: connectPeer,
}

func connectPeer(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	targetAddress := ctx.Args().Get(0)
	splitAddr := strings.Split(targetAddress, "@")
	if len(splitAddr) != 2 {
		return fmt.Errorf("target address expected in format: " +
			"pubkey@host:port")
	}

	addr := &lnrpc.LightningAddress{
		Pubkey: splitAddr[0],
		Host:   splitAddr[1],
	}
	req := &lnrpc.ConnectPeerRequest{
		Addr: addr,
		Perm: ctx.Bool("perm"),
	}

	lnid, err := client.ConnectPeer(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(lnid)
	return nil
}

// TODO(roasbeef): default number of confirmations
var OpenChannelCommand = cli.Command{
	Name: "openchannel",
	Description: "Attempt to open a new channel to an existing peer, " +
		"optionally blocking until the channel is 'open'. Once the " +
		"channel is open, a channelPoint (txid:vout) of the funding " +
		"output is returned. NOTE: peer_id and node_key are " +
		"mutually exclusive, only one should be used, not both.",
	Usage: "openchannel --node_key=X --local_amt=N --push_amt=N --num_confs=N",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "peer_id",
			Usage: "the relative id of the peer to open a channel with",
		},
		cli.StringFlag{
			Name: "node_key",
			Usage: "the identity public key of the target peer " +
				"serialized in compressed format",
		},
		cli.IntFlag{
			Name:  "local_amt",
			Usage: "the number of satoshis the wallet should commit to the channel",
		},
		cli.IntFlag{
			Name: "push_amt",
			Usage: "the number of satoshis to push to the remote " +
				"side as part of the initial commitment state",
		},
		cli.IntFlag{
			Name: "num_confs",
			Usage: "the number of confirmations required before the " +
				"channel is considered 'open'",
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block and wait until the channel is fully open",
		},
	},
	Action: openChannel,
}

func openChannel(ctx *cli.Context) error {
	// TODO(roasbeef): add deadline to context
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	if ctx.Int("peer_id") != 0 && ctx.String("node_key") != "" {
		return fmt.Errorf("both peer_id and lightning_id cannot be set " +
			"at the same time, only one can be specified")
	}

	req := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: int64(ctx.Int("local_amt")),
		PushSat:            int64(ctx.Int("push_amt")),
		NumConfs:           uint32(ctx.Int("num_confs")),
	}

	if ctx.Int("peer_id") != 0 {
		req.TargetPeerId = int32(ctx.Int("peer_id"))
	} else {
		nodePubHex, err := hex.DecodeString(ctx.String("node_key"))
		if err != nil {
			return fmt.Errorf("unable to decode lightning id: %v", err)
		}
		req.NodePubkey = nodePubHex
	}

	stream, err := client.OpenChannel(ctxb, req)
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		switch update := resp.Update.(type) {
		case *lnrpc.OpenStatusUpdate_ChanPending:
			txid, err := chainhash.NewHash(update.ChanPending.Txid)
			if err != nil {
				return err
			}

			printJson(struct {
				FundingTxid string `json:"funding_txid"`
			}{
				FundingTxid: txid.String(),
			},
			)

			if !ctx.Bool("block") {
				return nil
			}

		case *lnrpc.OpenStatusUpdate_ChanOpen:
			channelPoint := update.ChanOpen.ChannelPoint
			txid, err := chainhash.NewHash(channelPoint.FundingTxid)
			if err != nil {
				return err
			}

			index := channelPoint.OutputIndex
			printJson(struct {
				ChannelPoint string `json:"channel_point"`
			}{
				ChannelPoint: fmt.Sprintf("%v:%v", txid, index),
			},
			)
		}
	}

	return nil
}

// TODO(roasbeef): also allow short relative channel ID.
var CloseChannelCommand = cli.Command{
	Name: "closechannel",
	Description: "Close an existing channel. The channel can be closed either " +
		"cooperatively, or uncooperatively (forced).",
	Usage: "closechannel funding_txid output_index time_limit allow_force",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "funding_txid",
			Usage: "the txid of the channel's funding transaction",
		},
		cli.IntFlag{
			Name: "output_index",
			Usage: "the output index for the funding output of the funding " +
				"transaction",
		},
		cli.StringFlag{
			Name: "time_limit",
			Usage: "a relative deadline afterwhich the attempt should be " +
				"abandonded",
		},
		cli.BoolFlag{
			Name: "force",
			Usage: "after the time limit has passed, attempt an " +
				"uncooperative closure",
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block until the channel is closed",
		},
	},
	Action: closeChannel,
}

func closeChannel(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	txid, err := chainhash.NewHashFromStr(ctx.String("funding_txid"))
	if err != nil {
		return err
	}

	// TODO(roasbeef): implement time deadline within server
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: txid[:],
			OutputIndex: uint32(ctx.Int("output_index")),
		},
		Force: ctx.Bool("force"),
	}

	stream, err := client.CloseChannel(ctxb, req)
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		switch update := resp.Update.(type) {
		case *lnrpc.CloseStatusUpdate_ClosePending:
			closingHash := update.ClosePending.Txid
			txid, err := chainhash.NewHash(closingHash)
			if err != nil {
				return err
			}

			printJson(struct {
				ClosingTXID string `json:"closing_txid"`
			}{
				ClosingTXID: txid.String(),
			})

			if !ctx.Bool("block") {
				return nil
			}

		case *lnrpc.CloseStatusUpdate_ChanClose:
			closingHash := update.ChanClose.ClosingTxid
			txid, err := chainhash.NewHash(closingHash)
			if err != nil {
				return err
			}

			printJson(struct {
				ClosingTXID string `json:"closing_txid"`
			}{
				ClosingTXID: txid.String(),
			})
		}
	}

	return nil
}

var ListPeersCommand = cli.Command{
	Name:        "listpeers",
	Description: "List all active, currently connected peers.",
	Action:      listPeers,
}

func listPeers(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPeersRequest{}
	resp, err := client.ListPeers(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var WalletBalanceCommand = cli.Command{
	Name:        "walletbalance",
	Description: "compute and display the wallet's current balance",
	Usage:       "walletbalance --witness_only=[true|false]",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "witness_only",
			Usage: "if only witness outputs should be considered when " +
				"calculating the wallet's balance",
		},
	},
	Action: walletBalance,
}

func walletBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.WalletBalanceRequest{
		WitnessOnly: ctx.Bool("witness_only"),
	}
	resp, err := client.WalletBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var ChannelBalanceCommand = cli.Command{
	Name:        "channelbalance",
	Description: "returns the sum of the total available channel balance across all open channels",
	Action:      channelBalance,
}

func channelBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := client.ChannelBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var GetInfoCommand = cli.Command{
	Name:        "getinfo",
	Description: "returns basic information related to the active daemon",
	Action:      getInfo,
}

func getInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetInfoRequest{}
	resp, err := client.GetInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var PendingChannelsCommand = cli.Command{
	Name:        "pendingchannels",
	Description: "display information pertaining to pending channels",
	Usage:       "pendingchannels --status=[all|opening|closing]",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "open, o",
			Usage: "display the status of new pending channels",
		},
		cli.BoolFlag{
			Name:  "close, c",
			Usage: "display the status of channels being closed",
		},
		cli.BoolFlag{
			Name: "all, a",
			Usage: "display the status of channels in the " +
				"process of being opened or closed",
		},
	},
	Action: pendingChannels,
}

func pendingChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var channelStatus lnrpc.ChannelStatus
	switch {
	case ctx.Bool("all"):
		channelStatus = lnrpc.ChannelStatus_ALL
	case ctx.Bool("open"):
		channelStatus = lnrpc.ChannelStatus_OPENING
	case ctx.Bool("close"):
		channelStatus = lnrpc.ChannelStatus_CLOSING
	default:
		channelStatus = lnrpc.ChannelStatus_ALL
	}

	req := &lnrpc.PendingChannelRequest{channelStatus}
	resp, err := client.PendingChannels(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)

	return nil
}

var ListChannelsCommand = cli.Command{
	Name:        "listchannels",
	Description: "list all open channels",
	Usage:       "listchannels --active_only",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "active_only, a",
			Usage: "only list channels which are currently active",
		},
	},
	Action: listChannels,
}

func listChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListChannelsRequest{}
	resp, err := client.ListChannels(ctxb, req)
	if err != nil {
		return err
	}

	// TODO(roasbeef): defer close the client for the all

	printRespJson(resp)

	return nil
}

var SendPaymentCommand = cli.Command{
	Name:        "sendpayment",
	Description: "send a payment over lightning",
	Usage:       "sendpayment --dest=[node_key] --amt=[in_satoshis] --payment_hash=[hash] --debug_send=[true|false]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest, d",
			Usage: "the compressed identity pubkey of the " +
				"payment recipient",
		},
		cli.IntFlag{ // TODO(roasbeef): float64?
			Name:  "amt, a",
			Usage: "number of satoshis to send",
		},
		cli.StringFlag{
			Name:  "payment_hash, r",
			Usage: "the hash to use within the payment's HTLC",
		},
		cli.BoolFlag{
			Name:  "debug_send",
			Usage: "use the debug rHash when sending the HTLC",
		},
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "a zbase32-check encoded payment request to fulfill",
		},
	},
	Action: sendPaymentCommand,
}

func sendPaymentCommand(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var req *lnrpc.SendRequest
	if ctx.String("pay_req") != "" {
		req = &lnrpc.SendRequest{
			PaymentRequest: ctx.String("pay_req"),
		}
	} else {
		destNode, err := hex.DecodeString(ctx.String("dest"))
		if err != nil {
			return err
		}
		if len(destNode) != 33 {
			return fmt.Errorf("dest node pubkey must be exactly 33 bytes, is "+
				"instead: %v", len(destNode))
		}

		req = &lnrpc.SendRequest{
			Dest: destNode,
			Amt:  int64(ctx.Int("amt")),
		}

		if !ctx.Bool("debug_send") {
			rHash, err := hex.DecodeString(ctx.String("payment_hash"))
			if err != nil {
				return err
			}
			if len(rHash) != 32 {
				return fmt.Errorf("payment hash must be exactly 32 "+
					"bytes, is instead %v", len(rHash))
			}
			req.PaymentHash = rHash
		}
	}

	paymentStream, err := client.SendPayment(context.Background())
	if err != nil {
		return err
	}

	if err := paymentStream.Send(req); err != nil {
		return err
	}

	resp, err := paymentStream.Recv()
	if err != nil {
		return err
	}

	paymentStream.CloseSend()

	printRespJson(resp)

	return nil
}

var AddInvoiceCommand = cli.Command{
	Name:        "addinvoice",
	Description: "add a new invoice, expressing intent for a future payment",
	Usage:       "addinvoice --memo=[note] --receipt=[sig+contract hash] --value=[in_satoshis] --preimage=[32_byte_hash]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "memo",
			Usage: "an optional memo to attach along with the invoice",
		},
		cli.StringFlag{
			Name:  "receipt",
			Usage: "an optional cryptographic receipt of payment",
		},
		cli.StringFlag{
			Name:  "preimage",
			Usage: "the hex-encoded preimage which will allow settling an incoming HTLC payable to this preimage",
		},
		cli.IntFlag{
			Name:  "value",
			Usage: "the value of this invoice in satoshis",
		},
	},
	Action: addInvoice,
}

func addInvoice(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	preimage, err := hex.DecodeString(ctx.String("preimage"))
	if err != nil {
		return fmt.Errorf("unable to parse preimage: %v", err)
	}

	receipt, err := hex.DecodeString(ctx.String("receipt"))
	if err != nil {
		return fmt.Errorf("unable to parse receipt: %v", err)
	}

	invoice := &lnrpc.Invoice{
		Memo:      ctx.String("memo"),
		Receipt:   receipt,
		RPreimage: preimage,
		Value:     int64(ctx.Int("value")),
	}

	resp, err := client.AddInvoice(context.Background(), invoice)
	if err != nil {
		return err
	}

	printJson(struct {
		RHash  string `json:"r_hash"`
		PayReq string `json:"pay_req"`
	}{
		RHash:  hex.EncodeToString(resp.RHash),
		PayReq: resp.PaymentRequest,
	})

	return nil
}

var LookupInvoiceCommand = cli.Command{
	Name:        "lookupinvoice",
	Description: "lookup an existing invoice by its payment hash",
	Usage:       "lookupinvoice --rhash=[32_byte_hash]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "rhash",
			Usage: "the payment hash of the invoice to query for, the hash " +
				"should be a hex-encoded string",
		},
	},
	Action: lookupInvoice,
}

func lookupInvoice(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	rHash, err := hex.DecodeString(ctx.String("rhash"))
	if err != nil {
		return err
	}

	req := &lnrpc.PaymentHash{
		RHash: rHash,
	}

	invoice, err := client.LookupInvoice(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(invoice)

	return nil
}

var ListInvoicesCommand = cli.Command{
	Name:        "listinvoices",
	Usage:       "listinvoice --pending_only=[true|false]",
	Description: "list all invoices currently stored",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "pending_only",
			Usage: "toggles if all invoices should be returned, or only " +
				"those that are currently unsettled",
		},
	},
	Action: listInvoices,
}

func listInvoices(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	pendingOnly := true
	if !ctx.Bool("pending_only") {
		pendingOnly = false
	}

	req := &lnrpc.ListInvoiceRequest{
		PendingOnly: pendingOnly,
	}

	invoices, err := client.ListInvoices(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(invoices)

	return nil
}

var DescribeGraphCommand = cli.Command{
	Name: "describegraph",
	Description: "prints a human readable version of the known channel " +
		"graph from the PoV of the node",
	Usage: "describegraph",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "render",
			Usage: "If true, then an image of graph will be generated and displayed. The generated image is stored within the current directory with a file name of 'graph.svg'",
		},
	},
	Action: describeGraph,
}

func describeGraph(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelGraphRequest{}

	graph, err := client.DescribeGraph(context.Background(), req)
	if err != nil {
		return err
	}

	// If the draw flag is on, then we'll use the 'dot' command to create a
	// visualization of the graph itself.
	if ctx.Bool("render") {
		return drawChannelGraph(graph)
	}

	printRespJson(graph)
	return nil
}

// normalizeFunc is a factory function which returns a function that normalizes
// the capacity of of edges within the graph. The value of the returned
// function can be used to either plot the capacities, or to use a weight in a
// rendering of the graph.
func normalizeFunc(edges []*lnrpc.ChannelEdge, scaleFactor float64) func(int64) float64 {
	var (
		min float64 = math.MaxInt64
		max float64
	)

	for _, edge := range edges {
		// In order to obtain saner values, we reduce the capacity of a
		// channel to it's base 2 logarithm.
		z := math.Log2(float64(edge.Capacity))

		if z < min {
			min = z
		}
		if z > max {
			max = z
		}
	}

	return func(x int64) float64 {
		y := math.Log2(float64(x))

		// TODO(roasbeef): results in min being zero
		return float64(y-min) / float64(max-min) * scaleFactor
	}
}

func drawChannelGraph(graph *lnrpc.ChannelGraph) error {
	// First we'll create a temporary file that we'll write the compiled
	// string that describes our graph in the dot format to.
	tempDotFile, err := ioutil.TempFile("", "")
	if err != nil {
		return err
	}
	defer os.Remove(tempDotFile.Name())

	// Next, we'll create (or re-create) the file that the final graph
	// image will be written to.
	imageFile, err := os.Create("graph.svg")
	if err != nil {
		return err
	}

	// With our temporary files set up, we'll initialize the graphviz
	// object that we'll use to draw our graph.
	graphName := "LightningNetwork"
	graphCanvas := gographviz.NewGraph()
	graphCanvas.SetName(graphName)
	graphCanvas.SetDir(false)

	const numKeyChars = 10

	truncateStr := func(k string, n uint) string {
		return k[:n]
	}

	// For each node within the graph, we'll add a new vertex to the graph.
	for _, node := range graph.Nodes {
		// Rather than using the entire hex-encoded string, we'll only
		// use the first 10 characters. We also add a prefix of "Z" as
		// graphviz is unable to parse the compressed pubkey as a
		// non-integer.
		//
		// TODO(roasbeef): should be able to get around this?
		nodeID := fmt.Sprintf(`"%v"`, truncateStr(node.PubKey, numKeyChars))

		graphCanvas.AddNode(graphName, nodeID, gographviz.Attrs{})
	}

	normalize := normalizeFunc(graph.Edges, 3)

	// Similarly, for each edge we'll add an edge between the corresponding
	// nodes added to the graph above.
	for _, edge := range graph.Edges {
		// Once again, we add a 'Z' prefix so we're compliant with the
		// dot grammar.
		src := fmt.Sprintf(`"%v"`, truncateStr(edge.Node1Pub, numKeyChars))
		dest := fmt.Sprintf(`"%v"`, truncateStr(edge.Node2Pub, numKeyChars))

		// The weight for our edge will be the total capacity of the
		// channel, in BTC.
		// TODO(roasbeef): can also factor in the edges time-lock delta
		// and fee information
		amt := btcutil.Amount(edge.Capacity).ToBTC()
		edgeWeight := strconv.FormatFloat(amt, 'f', -1, 64)

		// The label for each edge will simply be a truncated version
		// of it's channel ID.
		chanIDStr := strconv.FormatUint(edge.ChannelId, 10)
		edgeLabel := fmt.Sprintf(`"cid:%v"`, truncateStr(chanIDStr, 7))

		// We'll also use a normalized version of the channels'
		// capacity in satoshis in order to modulate the "thickness" of
		// the line that creates the edge within the graph.
		normalizedCapacity := normalize(edge.Capacity)
		edgeThickness := strconv.FormatFloat(normalizedCapacity, 'f', -1, 64)

		// TODO(roasbeef): color code based on percentile capacity
		graphCanvas.AddEdge(src, dest, false, gographviz.Attrs{
			"penwidth": edgeThickness,
			"weight":   edgeWeight,
			"label":    edgeLabel,
		})
	}

	// With the declarative generation of the graph complete, we now write
	// the dot-string description of the graph
	graphDotString := graphCanvas.String()
	if _, err := tempDotFile.WriteString(graphDotString); err != nil {
		return err
	}
	if err := tempDotFile.Sync(); err != nil {
		return err
	}

	var errBuffer bytes.Buffer

	// Once our dot file has been written to disk, we can use the dot
	// command itself to generate the drawn rendering of the graph
	// described.
	drawCmd := exec.Command("dot", "-T"+"svg", "-o"+imageFile.Name(),
		tempDotFile.Name())
	drawCmd.Stderr = &errBuffer
	if err := drawCmd.Run(); err != nil {
		fmt.Println("error rendering graph: ", errBuffer.String())
		fmt.Println("dot: ", graphDotString)

		return err
	}

	errBuffer.Reset()

	// Finally, we'll open the drawn graph to display to the user.
	openCmd := exec.Command("open", imageFile.Name())
	openCmd.Stderr = &errBuffer
	if err := openCmd.Run(); err != nil {
		fmt.Println("error opening rendered graph image: ",
			errBuffer.String())
		return err
	}

	return nil
}

var ListPaymentsCommand = cli.Command{
	Name:        "listpayments",
	Usage:       "listpayments",
	Description: "list all outgoing payments",
	Action:      listPayments,
}

func listPayments(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPaymentsRequest{}

	payments, err := client.ListPayments(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(payments)
	return nil
}

var GetChanInfoCommand = cli.Command{
	Name:  "getchaninfo",
	Usage: "getchaninfo --chan_id=[8_byte_channel_id]",
	Description: "prints out the latest authenticated state for a " +
		"particular channel",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "chan_id",
			Usage: "the 8-byte compact channel ID to query for",
		},
	},
	Action: getChanInfo,
}

func getChanInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChanInfoRequest{
		ChanId: uint64(ctx.Int("chan_id")),
	}

	chanInfo, err := client.GetChanInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(chanInfo)
	return nil
}

var GetNodeInfoCommand = cli.Command{
	Name:  "getnodeinfo",
	Usage: "getnodeinfo --pub_key=[33_byte_serialized_pub_lky]",
	Description: "prints out the latest authenticated node state for an " +
		"advertised node",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "pub_key",
			Usage: "the 33-byte hex-encoded compressed public of the target " +
				"node",
		},
	},
	Action: getNodeInfo,
}

func getNodeInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NodeInfoRequest{
		PubKey: ctx.String("pub_key"),
	}

	nodeInfo, err := client.GetNodeInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(nodeInfo)
	return nil
}

var QueryRouteCommand = cli.Command{
	Name:        "queryroute",
	Usage:       "queryroute --dest=[dest_pub_key] --amt=[amt_to_send_in_satoshis]",
	Description: "queries the channel router for a potential path to the destination that has sufficient flow for the amount including fees",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest",
			Usage: "the 33-byte hex-encoded public key for the payment " +
				"destination",
		},
		cli.IntFlag{
			Name:  "amt",
			Usage: "the amount to send expressed in satoshis",
		},
	},
	Action: queryRoute,
}

func queryRoute(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.RouteRequest{
		PubKey: ctx.String("dest"),
		Amt:    int64(ctx.Int("amt")),
	}

	route, err := client.QueryRoute(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(route)
	return nil
}

var GetNetworkInfoCommand = cli.Command{
	Name:  "getnetworkinfo",
	Usage: "getnetworkinfo",
	Description: "returns a set of statistics pertaining to the known channel " +
		"graph",
	Action: getNetworkInfo,
}

func getNetworkInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NetworkInfoRequest{}

	netInfo, err := client.GetNetworkInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(netInfo)
	return nil
}

var DebugLevel = cli.Command{
	Name:        "debuglevel",
	Usage:       "debuglevel [--show|--level=<level_spec>]",
	Description: "Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "show",
			Usage: "if true, then the list of available sub-systems will be printed out",
		},
		cli.StringFlag{
			Name:  "level",
			Usage: "the level specification to target either a coarse logging level, or granular set of specific sub-systems with loggin levels for each",
		},
	},
	Action: debugLevel,
}

func debugLevel(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.DebugLevelRequest{
		Show:      ctx.Bool("show"),
		LevelSpec: ctx.String("level"),
	}

	resp, err := client.DebugLevel(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var DecodePayReq = cli.Command{
	Name:        "decodepayreq",
	Usage:       "decodepayreq --pay_req=[encoded_pay_req]",
	Description: "Decode the passed payment request revealing the destination, payment hash and value of the payment request",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "the zpay32 encoded payment request",
		},
	},
	Action: decodePayReq,
}

func decodePayReq(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	if ctx.String("pay_req") == "" {
		return errors.New("the --pay_req argument cannot be empty")
	}

	resp, err := client.DecodePayReq(ctxb, &lnrpc.PayReqString{
		PayReq: ctx.String("pay_req"),
	})
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}
