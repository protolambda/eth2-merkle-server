module eth2-merkle-server

go 1.15

require (
	github.com/protolambda/eth2api v0.0.0-20201203020746-3606d1957f92
	github.com/protolambda/zrnt v0.13.2
	github.com/protolambda/ztyp v0.1.2
)

replace (
	github.com/protolambda/zrnt => ../zrnt
	github.com/protolambda/ztyp => ../ztyp
)
