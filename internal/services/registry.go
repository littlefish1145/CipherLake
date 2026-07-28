package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// ServiceRegistry handles Consul-based service registration and discovery
type ServiceRegistry struct {
	client    *api.Client
	serviceID string
	mu        sync.RWMutex
}

// ServiceConfig defines configuration for a microservice
type ServiceConfig struct {
	Name          string
	ID            string
	Address       string
	Port          int
	Tags          []string
	TTL           time.Duration
	CheckInterval time.Duration
}

// NewServiceRegistry creates a new Consul-based service registry
func NewServiceRegistry(consulAddr string) (*ServiceRegistry, error) {
	config := api.DefaultConfig()
	if consulAddr != "" {
		config.Address = consulAddr
	}

	client, err := api.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consul client: %w", err)
	}

	return &ServiceRegistry{
		client: client,
	}, nil
}

// Register registers a service with Consul
func (r *ServiceRegistry) Register(ctx context.Context, cfg ServiceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("%s-%d", cfg.Name, time.Now().UnixNano())
	}

	r.serviceID = cfg.ID

	registration := &api.AgentServiceRegistration{
		ID:      cfg.ID,
		Name:    cfg.Name,
		Address: cfg.Address,
		Port:    cfg.Port,
		Tags:    cfg.Tags,
		Check: &api.AgentServiceCheck{
			TTL:                            cfg.TTL.String(),
			CheckID:                        fmt.Sprintf("%s-health", cfg.ID),
			Status:                         api.HealthPassing,
			DeregisterCriticalServiceAfter: "5m",
		},
	}

	err := r.client.Agent().ServiceRegister(registration)
	if err != nil {
		return fmt.Errorf("failed to register service: %w", err)
	}

	zap.L().Info("service registered",
		zap.String("service_name", cfg.Name),
		zap.String("service_id", cfg.ID),
		zap.String("address", cfg.Address),
		zap.Int("port", cfg.Port))

	return nil
}

// Deregister removes a service from Consul
func (r *ServiceRegistry) Deregister(ctx context.Context) error {
	r.mu.RLock()
	serviceID := r.serviceID
	r.mu.RUnlock()

	if serviceID == "" {
		return nil
	}

	err := r.client.Agent().ServiceDeregister(serviceID)
	if err != nil {
		return fmt.Errorf("failed to deregister service: %w", err)
	}

	zap.L().Info("service deregistered", zap.String("service_id", serviceID))
	return nil
}

// UpdateTTL updates the TTL health check
func (r *ServiceRegistry) UpdateTTL(status string, output string) error {
	r.mu.RLock()
	serviceID := r.serviceID
	r.mu.RUnlock()

	if serviceID == "" {
		return nil
	}

	checkID := fmt.Sprintf("%s-health", serviceID)
	return r.client.Agent().UpdateTTL(checkID, output, status)
}

// DiscoverService finds a service by name
func (r *ServiceRegistry) DiscoverService(ctx context.Context, serviceName string) (*ServiceInstance, error) {
	services, _, err := r.client.Health().Service(serviceName, "", true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to discover service: %w", err)
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no healthy instances found for service %s", serviceName)
	}

	// Return the first healthy instance
	service := services[0]
	return &ServiceInstance{
		ID:      service.Service.ID,
		Name:    service.Service.Service,
		Address: service.Service.Address,
		Port:    service.Service.Port,
		Tags:    service.Service.Tags,
	}, nil
}

// DiscoverAllServices finds all instances of a service
func (r *ServiceRegistry) DiscoverAllServices(ctx context.Context, serviceName string) ([]*ServiceInstance, error) {
	services, _, err := r.client.Health().Service(serviceName, "", true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to discover service: %w", err)
	}

	instances := make([]*ServiceInstance, 0, len(services))
	for _, service := range services {
		instances = append(instances, &ServiceInstance{
			ID:      service.Service.ID,
			Name:    service.Service.Service,
			Address: service.Service.Address,
			Port:    service.Service.Port,
			Tags:    service.Service.Tags,
		})
	}

	return instances, nil
}

// ServiceInstance represents a discovered service instance
type ServiceInstance struct {
	ID      string
	Name    string
	Address string
	Port    int
	Tags    []string
}

// AddressString returns the address as a string (host:port)
func (s *ServiceInstance) AddressString() string {
	return fmt.Sprintf("%s:%d", s.Address, s.Port)
}

// MTLSConfig manages mTLS configuration for service-to-service communication
type MTLSConfig struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	ServerName string
}

// LoadTLSConfig loads TLS configuration for mTLS
func (m *MTLSConfig) LoadTLSConfig() (*tls.Config, error) {
	// Load server certificate
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	// Load CA certificate for client verification
	caCert, err := os.ReadFile(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
		MinVersion:   tls.VersionTLS12,
		ServerName:   m.ServerName,
	}, nil
}

// LoadClientTLSConfig loads TLS configuration for client connections
func (m *MTLSConfig) LoadClientTLSConfig() (*tls.Config, error) {
	// Load client certificate
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}

	// Load CA certificate for server verification
	caCert, err := os.ReadFile(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
		ServerName:   m.ServerName,
	}, nil
}
