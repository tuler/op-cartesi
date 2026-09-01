module github.com/tuler/op-cartesi/lib/abi-drive/go

go 1.25.0

require github.com/tuler/op-cartesi/lib/accounts-drive/go v0.0.0-00010101000000-000000000000

require (
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/tuler/op-cartesi/lib/accounts-drive/go => ../../accounts-drive/go
