# Eth2 merkle server

**work in progress**

Fun experiment:
- keep chain in sync via external Eth2 beacon API
- process all blocks, calculate new state as blocks come in
- represent state efficiently with binary trees: lots more different and detailed state in memory at the same time
- serve HTTP API:
  - serve merkle proofs for arbitrary state (as long as it is recent-ish, like last two days? TBD.)
    - query: state/block root + merkle path(s) (generalized indices!)
    - response: merkle (multi) proof of state data
- bonus:
  - process attestations that were included in blocks
  - keep track of fork-choice
  - API to check voting of validators, the block-tree with weights, etc.

Built on top of:
- [`protolambda/ZRNT`](https://github.com/protolambda/zrnt): state, eth2 types, state-transitions, fork-choice
- [`protolambda/ZTYP`](https://github.com/protolambda/ztyp): SSZ, binary merkle trees, typing
- [`protolambda/eth2api`](https://github.com/protolambda/eth2api): Go API client bindings for Eth2
- [`protolambda/rumor`](https://github.com/protolambda/rumor): copy/modifications of chain and state/block DB code, to be refactored into its own repo later.


## License

MIT, see [`LICENSE`](./LICENSE) file.
