package tomox

import (
	"github.com/69th-byte/sdexchain/accounts/abi/bind"
	"github.com/69th-byte/sdexchain/common"
	"github.com/69th-byte/sdexchain/contracts/tomox/contract"
)

type TOMOXListing struct {
	*contract.TOMOXListingSession
	contractBackend bind.ContractBackend
}

func NewMyTOMOXListing(transactOpts *bind.TransactOpts, contractAddr common.Address, contractBackend bind.ContractBackend) (*TOMOXListing, error) {
	smartContract, err := contract.NewTOMOXListing(contractAddr, contractBackend)
	if err != nil {
		return nil, err
	}

	return &TOMOXListing{
		&contract.TOMOXListingSession{
			Contract:     smartContract,
			TransactOpts: *transactOpts,
		},
		contractBackend,
	}, nil
}

func DeployTOMOXListing(transactOpts *bind.TransactOpts, contractBackend bind.ContractBackend) (common.Address, *TOMOXListing, error) {
	contractAddr, _, _, err := contract.DeployTOMOXListing(transactOpts, contractBackend)
	if err != nil {
		return contractAddr, nil, err
	}
	smartContract, err := NewMyTOMOXListing(transactOpts, contractAddr, contractBackend)
	if err != nil {
		return contractAddr, nil, err
	}

	return contractAddr, smartContract, nil
}
