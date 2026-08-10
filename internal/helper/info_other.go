//go:build !linux

package helper

func currentHelperInfo() (Info, int, func(), error) {
	return Info{ProtocolVersion: protocolVersion}, -1, nil, nil
}
