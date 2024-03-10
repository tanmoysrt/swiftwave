package worker

import (
	"context"
	"errors"
	haproxymanager "github.com/swiftwave-org/swiftwave/haproxy_manager"
	"github.com/swiftwave-org/swiftwave/swiftwave_service/core"
	"github.com/swiftwave-org/swiftwave/swiftwave_service/manager"
	UDP_PROXY "github.com/swiftwave-org/swiftwave/udp_proxy_manager"
	"gorm.io/gorm"
	"log"
)

func (m Manager) IngressRuleDelete(request IngressRuleDeleteRequest, ctx context.Context, cancelContext context.CancelFunc) error {
	dbWithoutTx := m.ServiceManager.DbClient
	// restricted ports
	restrictedPorts := make([]int, 0)
	for _, port := range m.Config.SystemConfig.RestrictedPorts {
		restrictedPorts = append(restrictedPorts, int(port))
	}
	// fetch ingress rule
	var ingressRule core.IngressRule
	err := ingressRule.FindById(ctx, dbWithoutTx, request.Id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	// check status should be deleting
	if ingressRule.Status != core.IngressRuleStatusDeleting {
		// dont requeue
		return nil
	}
	// fetch the domain
	domain := core.Domain{}
	if ingressRule.Protocol == core.HTTPProtocol || ingressRule.Protocol == core.HTTPSProtocol {
		if ingressRule.DomainID == nil {
			return errors.New("domain id is nil")
		}
		err = domain.FindById(ctx, dbWithoutTx, *ingressRule.DomainID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
	}

	// fetch application
	var application core.Application
	err = application.FindById(ctx, dbWithoutTx, ingressRule.ApplicationID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	// fetch all proxy servers
	proxyServers, err := core.FetchProxyActiveServers(&m.ServiceManager.DbClient)
	if err != nil {
		return err
	}
	// fetch all haproxy managers
	haproxyManagers, err := manager.HAProxyClients(context.Background(), proxyServers)
	if err != nil {
		return err
	}
	// fetch all udp proxy managers
	udpProxyManagers, err := manager.UDPProxyClients(context.Background(), proxyServers)
	if err != nil {
		return err
	}
	// map of server ip and transaction id
	transactionIdMap := make(map[*haproxymanager.Manager]string)
	isFailed := false

	for _, haproxyManager := range haproxyManagers {
		// generate backend name
		backendName := haproxyManager.GenerateBackendName(application.Name, int(ingressRule.TargetPort))
		// delete ingress rule from haproxy
		// create new haproxy transaction
		haproxyTransactionId, err := haproxyManager.FetchNewTransactionId()
		if err != nil {
			return err
		}
		// delete ingress rule
		if ingressRule.Protocol == core.HTTPSProtocol {
			err = haproxyManager.DeleteHTTPSLink(haproxyTransactionId, backendName, domain.Name)
			if err != nil {
				// set status as failed and exit
				// because `DeleteHTTPSLink` can fail only if haproxy not working
				isFailed = true
				// requeue required as it fault of haproxy and may be resolved in next try
				return err
			}
		} else if ingressRule.Protocol == core.HTTPProtocol {
			if ingressRule.Port == 80 {
				err = haproxyManager.DeleteHTTPLink(haproxyTransactionId, backendName, domain.Name)
				if err != nil {
					// set status as failed and exit
					// because `DeleteHTTPLink` can fail only if haproxy not working
					isFailed = true
					// requeue required as it fault of haproxy and may be resolved in next try
					return err
				}
			} else {
				err = haproxyManager.DeleteTCPLink(haproxyTransactionId, backendName, int(ingressRule.Port), domain.Name, restrictedPorts)
				if err != nil {
					// set status as failed and exit
					// because `DeleteTCPLink` can fail only if haproxy not working
					isFailed = true
					// requeue required as it fault of haproxy and may be resolved in next try
					return err
				}
			}
		} else if ingressRule.Protocol == core.TCPProtocol {
			err = haproxyManager.DeleteTCPLink(haproxyTransactionId, backendName, int(ingressRule.Port), "", restrictedPorts)
			if err != nil {
				// set status as failed and exit
				// because `DeleteTCPLink` can fail only if haproxy not working
				isFailed = true
				// requeue required as it fault of haproxy and may be resolved in next try
				return err
			}
		} else if ingressRule.Protocol == core.UDPProtocol {
			// leave it for udp proxy
		} else {
			// unknown protocol
			isFailed = true
			return nil
		}

		// delete backend
		backendUsedByOther := true
		var ingressRuleCheck core.IngressRule
		err = m.ServiceManager.DbClient.Where("id != ? AND application_id = ? AND target_port = ?", ingressRule.ID, ingressRule.ApplicationID, ingressRule.TargetPort).First(&ingressRuleCheck).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				backendUsedByOther = false
			}
		}
		if !backendUsedByOther {
			err = haproxyManager.DeleteBackend(haproxyTransactionId, backendName)
			if err != nil {
				// set status as failed and exit
				// because `DeleteBackend` can fail only if haproxy not working
				isFailed = true
				// requeue required as it fault of haproxy and may be resolved in next try
				return err
			}
		}
	}

	// delete ingress rule from udp proxy
	for _, udpProxyManager := range udpProxyManagers {
		if ingressRule.Protocol == core.UDPProtocol {
			err = udpProxyManager.Remove(UDP_PROXY.Proxy{
				Port:       int(ingressRule.Port),
				TargetPort: int(ingressRule.TargetPort),
				Service:    application.Name,
			})
			if err != nil {
				// set status as failed and exit
				isFailed = true
				// requeue required as it fault of udp proxy and may be resolved in next try
				return err
			}
		}
	}

	for haproxyManager, haproxyTransactionId := range transactionIdMap {
		if !isFailed {
			// commit the haproxy transaction
			err = haproxyManager.CommitTransaction(haproxyTransactionId)
		}
		if isFailed || err != nil {
			log.Println("failed to commit haproxy transaction", err)
			err := haproxyManager.DeleteTransaction(haproxyTransactionId)
			if err != nil {
				log.Println("failed to rollback haproxy transaction", err)
			}
		}
	}
	manager.KillAllHAProxyConnections(haproxyManagers)
	manager.KillAllUDPProxyConnections(udpProxyManagers)

	// delete ingress rule from database
	err = ingressRule.Delete(ctx, dbWithoutTx, true)
	if err != nil {
		return err
	}

	return nil
}
