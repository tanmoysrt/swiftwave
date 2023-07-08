package Manager

import (
	"context"
	"time"

	"github.com/mholt/acmez"
	"github.com/mholt/acmez/acme"
	"gorm.io/gorm"
)

type Manager struct {
	ctx      context.Context
	account  acme.Account
	client   acmez.Client
	dbClient gorm.DB
	options  ManagerOptions
}

type ManagerOptions struct {
	Email                     string
	AccountPrivateKeyFilePath string
	DomainPrivateKeyStorePath string
	DomainFullChainStorePath  string
}

type http01Solver struct {
	dbClient gorm.DB
}

// GORM Models
type KeyAuthorizationToken struct {
	Token              string `gorm:"primaryKey"`
	AuthorizationToken string
}

type DomainSSLDetails struct {
	Domain       string `gorm:"primaryKey"`
	CreationDate time.Time
}
