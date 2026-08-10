//go:build !darwin && !linux

package cluster

import "net"

func systemInterfaces() ([]HostInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]HostInterface, 0, len(interfaces))
	for _, current := range interfaces {
		addresses, err := current.Addrs()
		if err == nil {
			result = append(result, HostInterface{Name: current.Name, Addrs: addresses})
		}
	}
	return result, nil
}

func systemRoute(net.IP) (HostRoute, error) { return HostRoute{}, nil }
