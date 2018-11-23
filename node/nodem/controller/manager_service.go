// Copyright (C) 2014-2018 Goodrain Co., Ltd.
// RAINBOND, Application Management Platform

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/clientv3"
	"github.com/goodrain/rainbond/cmd/node/option"
	"github.com/goodrain/rainbond/node/nodem/client"
	"github.com/goodrain/rainbond/node/nodem/healthy"
	"github.com/goodrain/rainbond/node/nodem/service"
)

//ManagerService manager service
type ManagerService struct {
	ctx            context.Context
	cancel         context.CancelFunc
	syncCtx        context.Context
	syncCancel     context.CancelFunc
	conf           *option.Conf
	ctr            Controller
	cluster        client.ClusterClient
	healthyManager healthy.Manager
	services       *[]*service.Service
	etcdcli        *clientv3.Client
}

//GetAllService get all service
func (m *ManagerService) GetAllService() (*[]*service.Service, error) {
	return m.services, nil
}

//Start  start and monitor all service
func (m *ManagerService) Start() error {
	logrus.Info("Starting node controller manager.")
	*m.services = service.LoadServicesFromLocal(m.conf.ServiceListFile)
	if m.conf.EnableInitStart {
		return m.ctr.InitStart(*m.services)
	}
	return nil
}

//Stop stop manager
func (m *ManagerService) Stop() error {
	m.cancel()
	return nil
}

//Online start all service of on the node
func (m *ManagerService) Online() error {
	logrus.Info("Doing node online by node controller manager")
	// registry local services endpoint into cluster manager
	hostIP := m.cluster.GetOptions().HostIP
	for _, s := range *m.services {
		logrus.Debug("Parse endpoints for service: ", s.Name)
		for _, end := range s.Endpoints {
			logrus.Debug("Discovery endpoints: ", end.Name)
			endpoint := toEndpoint(end, hostIP)
			oldEndpoints := m.cluster.GetEndpoints(end.Name)
			if exist := isExistEndpoint(oldEndpoints, endpoint); !exist {
				oldEndpoints = append(oldEndpoints, endpoint)
				m.cluster.SetEndpoints(end.Name, oldEndpoints)
			}
		}
	}
	if ok := m.ctr.CheckBeforeStart(); !ok {
		return nil
	}

	err := m.WriteServices()
	if err != nil {
		return err
	}

	// start all by systemctl start multi-user.target
	m.ctr.StartList(*m.services)
	m.StartSyncService()

	return nil
}

//Offline stop all service of on the node
func (m *ManagerService) Offline() error {
	logrus.Info("Doing node offline by node controller manager")
	// Anti-registry local services endpoint from cluster manager
	HostIP := m.cluster.GetOptions().HostIP
	services, _ := m.GetAllService()
	for _, s := range *services {
		for _, end := range s.Endpoints {
			logrus.Debug("Anti-registry endpoint: ", end.Name)
			endpoint := toEndpoint(end, HostIP)
			oldEndpoints := m.cluster.GetEndpoints(end.Name)
			if exist := isExistEndpoint(oldEndpoints, endpoint); exist {
				m.cluster.SetEndpoints(end.Name, rmEndpointFrom(oldEndpoints, endpoint))
			}
		}
	}

	m.StopSyncService()

	if err := m.ctr.StopList(*m.services); err != nil {
		return err
	}

	return nil
}

// synchronize all service status to as we expect
func (m *ManagerService) StartSyncService() {
	m.syncCtx, m.syncCancel = context.WithCancel(context.Background())

	for _, s := range *m.services {
		name := s.Name
		logrus.Info("Start watch status for service: ", name)
		w := m.healthyManager.WatchServiceHealthy(name)
		if w == nil {
			logrus.Error("Not found watcher of the service ", name)
			return
		}

		go func() {
			m.healthyManager.EnableWatcher(w.GetServiceName(), w.GetID())
			defer w.Close()

			for {
				select {
				case event := <-w.Watch():
					switch event.Status {
					case service.Stat_healthy:
						logrus.Debugf("is [%s] of service %s.", event.Status, event.Name)
					case service.Stat_unhealthy:
						logrus.Debugf("is [%s] of service %s %d times.", event.Status, event.Name, event.ErrorNumber)
						if event.ErrorNumber > 3 {
							logrus.Infof("is [%s] of service %s %d times and restart it.", event.Status, event.Name, event.ErrorNumber)
							// disable check healthy status of the service
							m.healthyManager.DisableWatcher(w.GetServiceName(), w.GetID())
							m.ctr.RestartService(event.Name)
							if !m.WaitStart(event.Name, time.Minute) {
								logrus.Errorf("Timeout restart service: ", event.Name)
							}
							// start check healthy status of the service
							m.healthyManager.EnableWatcher(w.GetServiceName(), w.GetID())
						}
					case service.Stat_death:
						logrus.Infof("is [%s] of service %s %d times and start it.", event.Status, event.Name, event.ErrorNumber)
						// disable check healthy status of the service
						m.healthyManager.DisableWatcher(w.GetServiceName(), w.GetID())
						m.ctr.StartService(event.Name)
						if !m.WaitStart(event.Name, time.Minute) {
							logrus.Error("Timeout start service: ", event.Name)
						}
						// start check healthy status of the service
						m.healthyManager.EnableWatcher(w.GetServiceName(), w.GetID())
					}
				case <-m.syncCtx.Done():
					return
				}
			}
		}()
	}
}

func (m *ManagerService) StopSyncService() {
	if m.syncCtx != nil {
		m.syncCancel()
	}
}

func (m *ManagerService) WaitStart(name string, duration time.Duration) bool {
	max := time.Now().Add(duration)
	t := time.Tick(time.Second * 3)

	for {
		if time.Now().After(max) {
			return false
		}
		status, err := m.healthyManager.GetCurrentServiceHealthy(name)
		if err != nil {
			logrus.Errorf("Can not get %s service current status: %v", name, err)
			<-t
			continue
		}
		logrus.Debugf("Check service %s current status: %s", name, status.Status)
		if status.Status == service.Stat_healthy {
			return true
		}
		<-t
	}
}

/*
1. reload services info from local file system
2. regenerate systemd config file and restart with config changes
3. start all newly added services
*/
func (m *ManagerService) ReLoadServices() error {
	services := service.LoadServicesFromLocal(m.conf.ServiceListFile)
	for _, ne := range services {
		exists := false
		for _, old := range *m.services {
			if ne.Name == old.Name {
				if !reflect.DeepEqual(ne, old) {
					logrus.Infof("Recreate service [%s]", ne.Name)
					if err := m.ctr.WriteConfig(ne); err == nil {
						m.ctr.EnableService(ne.Name)
						m.ctr.RestartService(ne.Name)
					}
				}
				exists = true
				break
			}
		}
		if !exists {
			logrus.Infof("Create service [%s]", ne.Name)
			if err := m.ctr.WriteConfig(ne); err == nil {
				m.ctr.EnableService(ne.Name)
				m.ctr.StartService(ne.Name)
			}
		}
	}
	*m.services = services
	return nil
}

//WriteServices write services
func (m *ManagerService) WriteServices() error {
	for _, s := range *m.services {
		if s.Name == "docker" {
			continue
		}
		err := m.ctr.WriteConfig(s)
		if err != nil {
			return err
		}
	}

	return nil
}

//RemoveServices remove services
func (m *ManagerService) RemoveServices() error {
	for _, s := range *m.services {
		if s.Name == "docker" {
			continue
		}
		m.ctr.DisableService(s.Name)
		m.ctr.RemoveConfig(s.Name)
	}

	return nil
}

func isExistEndpoint(etcdEndPoints []string, end string) bool {
	for _, v := range etcdEndPoints {
		if v == end {
			return true
		}
	}
	return false
}

func rmEndpointFrom(etcdEndPoints []string, end string) []string {
	endPoints := make([]string, 0, 5)
	for _, v := range etcdEndPoints {
		if v != end {
			endPoints = append(endPoints, v)
		}
	}
	return endPoints
}

func toEndpoint(reg *service.Endpoint, ip string) string {
	if reg.Protocol == "" {
		return fmt.Sprintf("%s:%s", ip, reg.Port)
	}
	return fmt.Sprintf("%s://%s:%s", reg.Protocol, ip, reg.Port)
}

//NewManagerService new controller manager
func NewManagerService(conf *option.Conf, healthyManager healthy.Manager) (*ManagerService, client.ClusterClient) {
	ctx, cancel := context.WithCancel(context.Background())
	cluster := client.NewClusterClient(conf)
	manager := &ManagerService{
		ctx:            ctx,
		cancel:         cancel,
		conf:           conf,
		cluster:        cluster,
		ctr:            NewController(conf, cluster),
		healthyManager: healthyManager,
		etcdcli:        conf.EtcdCli,
		services:       new([]*service.Service),
	}
	return manager, cluster
}
