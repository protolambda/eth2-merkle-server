package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/protolambda/eth2-merkle-server/chain"
	"github.com/protolambda/eth2-merkle-server/chain/db/blocks"
	"github.com/protolambda/eth2-merkle-server/chain/db/states"
	"github.com/protolambda/eth2api"
	"github.com/protolambda/eth2api/beaconapi"
	"github.com/protolambda/eth2api/debugapi"
	"github.com/protolambda/eth2api/nodeapi"
	"github.com/protolambda/zrnt/eth2/beacon"
	"github.com/protolambda/zrnt/eth2/configs"
	"github.com/protolambda/ztyp/codec"
	"github.com/protolambda/ztyp/tree"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	beaconAddr := flag.String("beacon-addr", "http://localhost:4000/", "Beacon API addr to use")
	srvAddr := flag.String("srv-addr", "localhost:5000", "Addr to serve app on")
	blocksPath := flag.String("blocks-path", "blocks", "blocks storage path")
	anchorPath := flag.String("anchor-path", "genesis.ssz", "state anchor path")

	flag.Parse()

	// TODO: can make this configurable
	spec := configs.Mainnet

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())

	go runApp(ctx, spec, *anchorPath, *beaconAddr, *blocksPath, *srvAddr)

	select {
	case <-interrupt:
		cancel()
		<-time.After(time.Second * 5)
	}
}

func runApp(ctx context.Context, spec *beacon.Spec,
	anchorPath string, beaconAddr string, blocksPath string, srvAddr string) {
	check := func(err error) {
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "failed to query genesis state: %v\n", err)
			os.Exit(1)
		}
	}

	state, err := loadStateFile(spec, anchorPath)
	if err != nil {
		fmt.Println("no local state, getting it from API instead now")
		state, err = loadStateApi(ctx, beaconAddr, spec)
		check(err)
		fmt.Println("got state from API, persisting it for later use")
		// save the state for later
		err = writeStateFile(state, anchorPath)
		check(err)
	}

	fmt.Println("anchor state ready!")

	stateDB := &states.MemDB{}

	// TODO: persist and load old chain, to not lose progress on restart
	ch, err := chainFromAnchor(spec, state, stateDB)
	check(err)

	blockDB := blocks.NewFileDB(spec, blocksPath)

	server := NewServer(srvAddr, spec, beaconAddr, blockDB, stateDB, ch)
	server.Start(ctx)
}

func writeStateFile(state *beacon.BeaconStateView, path string) error {
	f, err := os.Create(path)
	defer f.Close()
	if err != nil {
		return err
	}
	bufW := bufio.NewWriter(f)
	defer bufW.Flush()
	return state.Serialize(codec.NewEncodingWriter(bufW))
}

func loadStateFile(spec *beacon.Spec, path string) (*beacon.BeaconStateView, error) {
	f, err := os.Open(path)
	defer f.Close()
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return beacon.AsBeaconStateView(spec.BeaconState().Deserialize(codec.NewDecodingReader(f, uint64(info.Size()))))
}

func loadStateApi(ctx context.Context, apiAddr string, spec *beacon.Spec) (*beacon.BeaconStateView, error) {
	cli := &eth2api.HttpClient{
		Addr: apiAddr,
		Cli: &http.Client{
			Timeout: time.Second * 20,
		},
	}
	var state beacon.BeaconState
	if exists, err := debugapi.BeaconState(ctx, cli, eth2api.StateIdSlot(0), &state); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to query genesis state: %v\n", err)
		os.Exit(1)
	} else if !exists {
		_, _ = fmt.Fprintf(os.Stderr, "node does not have a genesis state for us: %v\n", err)
		os.Exit(1)
	}

	return stateStructToView(spec, &state)
}

type Server struct {
	addr    string
	spec    *beacon.Spec
	blockDB blocks.DB
	stateDB states.DB
	chain   chain.FullChain
	apiCli  *eth2api.HttpClient
}

func NewServer(srvAddr string, spec *beacon.Spec, apiAddr string, blockDB blocks.DB, stateDB states.DB, chain chain.FullChain) *Server {
	return &Server{
		addr:    srvAddr,
		spec:    spec,
		blockDB: blockDB,
		stateDB: stateDB,
		chain:   chain,
		apiCli: &eth2api.HttpClient{
			Addr: apiAddr,
			Cli: &http.Client{
				Timeout: time.Second * 10,
			},
		}}
}

func (s *Server) Start(ctx context.Context) {
	router := http.NewServeMux()
	srv := &http.Server{
		Addr:    s.addr,
		Handler: router,
	}
	router.Handle("/block_roots", http.HandlerFunc(s.getBlockRoots))

	go func() {
		// accept connections
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("client hub server err: ", err)
		}
	}()
	if err := s.processLoop(ctx); err != nil {
		log.Println("processLoop failed: ", err)
	}
	fmt.Println("done processing, stopping server")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Print("Server shutdown failed: ", err)
	}
}

func (s *Server) processLoop(ctx context.Context) error {
	slotTicker := time.NewTicker(time.Second * 12)
	defer slotTicker.Stop()

	for {
		fmt.Println("loop iter")
		select {
		case <-slotTicker.C:
			if err := s.syncApi(ctx, 2000); err != nil { // TODO
				fmt.Printf("ERROR: failed to run sync: %v\n", err)
			}
			fmt.Println("tick")
		case <-ctx.Done():
			fmt.Printf("process loop stopped: %v\n", ctx.Err())
			return nil
		}
	}
}

func (s *Server) syncApi(ctx context.Context, maxSlot beacon.Slot) error {
	var syncStatus eth2api.SyncingStatus
	if err := nodeapi.SyncingStatus(ctx, s.apiCli, &syncStatus); err != nil {
		return fmt.Errorf("failed to check sync status: %v", err)
	}
	// sanity check
	if syncStatus.SyncDistance > syncStatus.HeadSlot {
		return fmt.Errorf("bad sync distance: %d, head: %d", syncStatus.SyncDistance, syncStatus.HeadSlot)
	}
	// try sync up, until it doesn't know the block anymore, or until we don't need more.
	head, err := s.chain.Head()
	if err != nil {
		return fmt.Errorf("could not get head: %v", err)
	}
	for i := head.Slot() + 1; i < syncStatus.HeadSlot && i < maxSlot; i++ {
		var blockHeaderInfo eth2api.BeaconBlockHeaderAndInfo
		if exists, err := beaconapi.BlockHeader(ctx, s.apiCli, eth2api.BlockIdSlot(i), &blockHeaderInfo); err != nil {
			return fmt.Errorf("failed to get block at slot %d from API", i)
		} else if !exists {
			// gap slot
			continue
		}
		blockHeader := &blockHeaderInfo.Header
		root := blockHeader.Message.HashTreeRoot(tree.GetHashFn())
		if blockHeader.Message.Slot > i {
			return fmt.Errorf("block %s at query slot %d was higher than expected slot %d",
				root.String(), i, blockHeader.Message.Slot)
		} else if blockHeader.Message.Slot < i {
			// gap slot, getting old block again. (API problem in lighthouse)
			continue
		}
		// TODO: reorg detection

		fmt.Printf("processing block %s at slot %d\n", root.String(), i)
		if _, err := s.chain.ByBlockRoot(root); err == nil {
			// we already know the block, skip it
			fmt.Printf("skipping block, chain already has block %s at slot %d\n", root.String(), i)
			continue
		}
		var block beacon.SignedBeaconBlock
		// try to get from local DB first
		if exists, err := s.blockDB.Get(ctx, root, &block); err != nil {
			return fmt.Errorf("db read error: %v", err)
		} else if !exists {
			// try API otherwise
			if exists, err := beaconapi.Block(ctx, s.apiCli, eth2api.BlockIdSlot(i), &block); err != nil {
				return fmt.Errorf("failed to get block at slot %d from API", i)
			} else if !exists {
				return fmt.Errorf("block with header could not be found: %s, slot: %d", root.String(), i)
			}

			// store block for later time.
			if exists, err := s.blockDB.Store(ctx, &blocks.BlockWithRoot{
				Root:  root,
				Block: &block,
			}); err != nil {
				return fmt.Errorf("failed to store block %s (slot %d)", root.String(), i)
			} else if exists {
				fmt.Printf("WARNING: unexpectedly loaded block from API that we already know: %s\n", root.String())
			}
		}

		// Add block to the chain (and do block processing)
		if err := s.chain.AddBlock(ctx, &block); err != nil {
			return fmt.Errorf("failed to add block %s (slot %d) to chain: %v", root.String(), i, err)
		}
		// Add the attestations of the block to the chain (we should have committee info etc. ready
		// since we just processed the block that includes the attestations)
		for i := 0; i < len(block.Message.Body.Attestations); i++ {
			if err := s.chain.AddAttestation(&block.Message.Body.Attestations[i]); err != nil {
				fmt.Printf("WARNING: failed to add attestation %d of block %s to chain: %v\n", i, root.String(), err)
			}
		}
	}
	return nil
}

func (s *Server) getBlockRoots(w http.ResponseWriter, r *http.Request) {
	//r.URL.Query().Get("")
	iter, err := s.chain.Iter()
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte("failed to get chain data"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	enc := json.NewEncoder(w)
	start := iter.Start()
	end := iter.End()
	roots := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		entry, err := iter.Entry(i)
		if err != nil {
			fmt.Printf("Warning: iter error at slot %d: %v\n", i, err)
			roots = append(roots, "(error)")
			continue
		}
		if entry.IsEmpty() {
			roots = append(roots, "null")
		} else {
			roots = append(roots, entry.BlockRoot().String())
		}
	}
	if err := enc.Encode(roots); err != nil {
		log.Printf("failed to write http response")
	}
}

// hack to convert one state representation type to another: serialize, then deserialize
func stateStructToView(spec *beacon.Spec, state *beacon.BeaconState) (*beacon.BeaconStateView, error) {
	var buf bytes.Buffer
	if err := state.Serialize(spec, codec.NewEncodingWriter(&buf)); err != nil {
		return nil, err
	}
	out, err := beacon.AsBeaconStateView(spec.BeaconState().Deserialize(codec.NewDecodingReader(&buf, uint64(buf.Len()))))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func chainFromAnchor(spec *beacon.Spec, anchorState *beacon.BeaconStateView, db states.DB) (chain.FullChain, error) {
	slot, err := anchorState.Slot()
	if err != nil {
		return nil, err
	}
	latestHeader, err := anchorState.LatestBlockHeader()
	if err != nil {
		return nil, err
	}
	latestHeader, err = beacon.AsBeaconBlockHeader(latestHeader.Copy())
	if err != nil {
		return nil, err
	}
	headerStateRoot, err := latestHeader.StateRoot()
	if err != nil {
		return nil, err
	}
	if headerStateRoot == (beacon.Root{}) {
		if err := latestHeader.SetStateRoot(anchorState.HashTreeRoot(tree.GetHashFn())); err != nil {
			return nil, err
		}
	}
	blockRoot := latestHeader.HashTreeRoot(tree.GetHashFn())
	parentRoot, err := latestHeader.ParentRoot()
	if err != nil {
		return nil, err
	}
	epc, err := spec.NewEpochsContext(anchorState)
	if err != nil {
		return nil, err
	}
	anchor := chain.NewHotEntry(slot, blockRoot, parentRoot, anchorState, epc)
	fch, err := chain.NewHotColdChain(anchor, spec, db)
	if err != nil {
		return nil, err
	}
	return fch, nil
}
