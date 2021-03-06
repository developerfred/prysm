package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path"

	stateTrie "github.com/prysmaticlabs/prysm/beacon-chain/state"

	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/state"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/benchutil"
	"github.com/prysmaticlabs/prysm/shared/interop"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

var (
	outputDir = flag.String("output-dir", "", "Directory to write SSZ files to")
	overwrite = flag.Bool("overwrite", false, "If SSZ files exist in the output directory, they will be overwritten")
)

func main() {
	flag.Parse()
	if *outputDir == "" {
		log.Fatal("Please specify --output-dir to write SSZ files to")
	}

	if !*overwrite {
		if _, err := os.Stat(path.Join(*outputDir, benchutil.BState1EpochFileName)); err == nil {
			log.Fatal("The file exists. Use a different file name or the --overwrite flag")
		}
		if _, err := os.Stat(path.Join(*outputDir, benchutil.BState2EpochFileName)); err == nil {
			log.Fatal("The file exists. Use a different file name or the --overwrite flag")
		}
		if _, err := os.Stat(path.Join(*outputDir, benchutil.FullBlockFileName)); err == nil {
			log.Fatal("The file exists. Use a different file name or the --overwrite flag")
		}
	}

	if err := os.MkdirAll(*outputDir, os.ModePerm); err != nil {
		log.Fatal(err)
	}

	log.Printf("Output dir is: %s", *outputDir)
	log.Println("Generating genesis state")
	// Generating this for the 2 following states.
	if err := generateGenesisBeaconState(); err != nil {
		log.Fatalf("Could not generate genesis state: %v", err)
	}
	log.Println("Generating full block and state after 1 skipped epoch")
	if err := generateMarshalledFullStateAndBlock(); err != nil {
		log.Fatalf("Could not generate full state and block: %v", err)
	}
	log.Println("Generating state after 2 fully attested epochs")
	if err := generate2FullEpochState(); err != nil {
		log.Fatalf("Could not generate 2 full epoch state: %v", err)
	}
	// Removing the genesis state SSZ since its 10MB large and no longer needed.
	if err := os.Remove(path.Join(*outputDir, benchutil.GenesisFileName)); err != nil {
		log.Fatal(err)
	}
}

func generateGenesisBeaconState() error {
	genesisState, _, err := interop.GenerateGenesisState(0, benchutil.ValidatorCount)
	if err != nil {
		return err
	}
	beaconBytes, err := ssz.Marshal(genesisState)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(*outputDir, benchutil.GenesisFileName), beaconBytes, 0644); err != nil {
		return err
	}
	return nil
}

func generateMarshalledFullStateAndBlock() error {
	benchutil.SetBenchmarkConfig()
	beaconState, err := genesisBeaconState()
	if err != nil {
		return err
	}

	privs, _, err := interop.DeterministicallyGenerateKeys(0, benchutil.ValidatorCount)
	if err != nil {
		return err
	}

	conf := &testutil.BlockGenConfig{}
	slotsPerEpoch := params.BeaconConfig().SlotsPerEpoch
	// Small offset for the beacon state so we dont process a block on an epoch.
	slotOffset := uint64(2)
	block, err := testutil.GenerateFullBlock(beaconState, privs, conf, slotsPerEpoch+slotOffset)
	if err != nil {
		return err
	}
	beaconState, err = state.ExecuteStateTransition(context.Background(), beaconState, block)
	if err != nil {
		return err
	}

	attConfig := &testutil.BlockGenConfig{
		NumAttestations: benchutil.AttestationsPerEpoch / slotsPerEpoch,
	}

	atts := []*ethpb.Attestation{}
	for i := slotOffset + 1; i < slotsPerEpoch+slotOffset; i++ {
		attsForSlot, err := testutil.GenerateAttestations(beaconState, privs, attConfig.NumAttestations, i)
		if err != nil {
			return err
		}
		atts = append(atts, attsForSlot...)
	}

	block, err = testutil.GenerateFullBlock(beaconState, privs, attConfig, beaconState.Slot())
	if err != nil {
		return errors.Wrap(err, "could not generate full block")
	}
	block.Block.Body.Attestations = append(atts, block.Block.Body.Attestations...)

	s, err := state.CalculateStateRoot(context.Background(), beaconState, block)
	if err != nil {
		return errors.Wrap(err, "could not calculate state root")
	}
	block.Block.StateRoot = s[:]
	blockRoot, err := ssz.HashTreeRoot(block.Block)
	if err != nil {
		return errors.Wrap(err, "could not get signing root of block")
	}
	// Temporarily incrementing the beacon state slot here since BeaconProposerIndex is a
	// function deterministic on beacon state slot.
	beaconState.SetSlot(beaconState.Slot() + 1)
	proposerIdx, err := helpers.BeaconProposerIndex(beaconState)
	if err != nil {
		return err
	}
	domain := helpers.Domain(beaconState.Fork(), helpers.CurrentEpoch(beaconState), params.BeaconConfig().DomainBeaconProposer)
	block.Signature = privs[proposerIdx].Sign(blockRoot[:], domain).Marshal()
	beaconState.SetSlot(beaconState.Slot() - 1)

	beaconBytes, err := ssz.Marshal(beaconState)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(*outputDir, benchutil.BState1EpochFileName), beaconBytes, 0644); err != nil {
		return err
	}

	// Running a single state transition to make sure the generated files aren't broken.
	_, err = state.ExecuteStateTransition(context.Background(), beaconState, block)
	if err != nil {
		return err
	}

	blockBytes, err := ssz.Marshal(block)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(*outputDir, benchutil.FullBlockFileName), blockBytes, 0644); err != nil {
		return err
	}
	return nil
}

func generate2FullEpochState() error {
	benchutil.SetBenchmarkConfig()
	beaconState, err := genesisBeaconState()
	if err != nil {
		return err
	}

	privs, _, err := interop.DeterministicallyGenerateKeys(0, benchutil.ValidatorCount)
	if err != nil {
		return err
	}

	attConfig := &testutil.BlockGenConfig{
		NumAttestations: benchutil.AttestationsPerEpoch / params.BeaconConfig().SlotsPerEpoch,
	}

	for i := uint64(0); i < params.BeaconConfig().SlotsPerEpoch*2-1; i++ {
		block, err := testutil.GenerateFullBlock(beaconState, privs, attConfig, beaconState.Slot())
		if err != nil {
			return err
		}
		beaconState, err = state.ExecuteStateTransition(context.Background(), beaconState, block)
		if err != nil {
			return err
		}
	}

	beaconBytes, err := ssz.Marshal(beaconState)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(*outputDir, benchutil.BState2EpochFileName), beaconBytes, 0644); err != nil {
		return err
	}
	return nil
}

func genesisBeaconState() (*stateTrie.BeaconState, error) {
	beaconBytes, err := ioutil.ReadFile(path.Join(*outputDir, benchutil.GenesisFileName))
	if err != nil {
		return nil, errors.Wrap(err, "cannot read genesis state file")
	}
	genesisState := &pb.BeaconState{}
	if err := ssz.Unmarshal(beaconBytes, genesisState); err != nil {
		return nil, errors.Wrap(err, "cannot unmarshal genesis state file")
	}
	return stateTrie.InitializeFromProtoUnsafe(genesisState)
}
