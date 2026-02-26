package whd

import "net"

// EthernetDevice is WIP of how ethernet device
// API design.
//
// Device-specific initialization (WiFi join, PHY auto-negotiation,
// firmware loading) must complete BEFORE the device is used as a stack endpoint.
type EthernetDevice interface {
	// SendEthFrame transmits a complete Ethernet frame.
	// The frame includes the Ethernet header but NOT the FCS/CRC
	// trailer (device or stack handles CRC as appropriate).
	// SendEthFrame blocks until the transmission is queued succesfully
	// or finished sending. Should not be called concurrently
	// unless user is sure the driver supports it.
	SendEthFrame(frame []byte) error

	// SetRecvHandler registers the function called when an Ethernet
	// frame is received.
	// We are still deciding on the API to be used. Should the handler be
	// called from an ISR context? Should the buffers be owned by
	// the device or the user? Does it help to provide buffers for the
	// device to use? WIP.
	SetEthRecvHandler(bufs [][]byte, handler func(pkt []byte) error)

	// EthPoll services the device. For poll-based devices (e.g. CYW43439
	// over SPI), reads from the bus and invokes the handler for each
	// received frame. Behaviour for interrupt driven devices is undefined
	// at the moment.
	// We are waiting to reach a decision on how CAN works in TinyGo
	// which will affect how we design this API: https://github.com/orgs/tinygo-org/discussions/5174
	// WIP.
	EthPoll() (bool, error)

	// HardwareAddr6 returns the device's 6-byte MAC address.
	// For PHY-only devices, returns the MAC provided at configuration.
	HardwareAddr6() ([6]byte, error)

	// MaxFrameSize returns the max complete Ethernet frame size
	// (including headers and any overhead) for buffer allocation.
	// MTU can be calculated doing:
	//  // mfu-(14+4+4) for:
	//  // ethernet header+ethernet CRC if present+ethernet VLAN overhead for VLAN support.
	//  mtu := dev.MaxFrameSize() - ethernet.MaxOverheadSize
	MaxFrameSize() int

	// NetFlags offers ability to provide user with notice of the device state.
	// May be also used to encode functioning such as if the device needs FCS/CRC encoding appended
	// to the ethernet packet. WIP.
	NetFlags() net.Flags
}

// JoinOptions configures WiFi connection parameters.
// See embassy-rs for reference: https://github.com/embassy-rs/embassy/blob/main/cyw43/src/control.rs
type JoinOptions struct {
	// Passphrase is the WiFi password.
	Passphrase string
	// Auth specifies the authentication method. Implementation will choose WPA2 if passphrase is set or Open if no passphrase is set.
	Auth JoinAuth
	// CipherNoAES disables AES cipher.
	CipherNoAES bool
	// CipherTKIP enables TKIP cipher. Default is false.
	CipherTKIP bool
}

// JoinAuth specifies the authentication method for joining a WiFi network.
type JoinAuth uint8

const (
	joinAuthUndefined JoinAuth = iota
	JoinAuthOpen
	JoinAuthWPA
	JoinAuthWPA2
	JoinAuthWPA3
	JoinAuthWPA2WPA3
)
