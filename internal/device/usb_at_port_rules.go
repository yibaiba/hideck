package device

import (
	"strings"

	"github.com/yibaiba/hideck/internal/modem"
)

const (
	cmIoTVendorID                = 0x2ecc
	ml307cProductID              = 0x3012
	quectelVendorID              = 0x2c7c
	qualcommVendorID             = 0x05c6
	quectel0125ProductID         = 0x0125
	quectelEC200UProductID       = 0x0901
	quectelEC200DProductID       = 0x0902
	quectelRG801HProductID       = 0x8101
	quectelRG500UProductID       = 0x0900
	quectelEC200TProductID       = 0x6026
	quectelEC200AProductID       = 0x6005
	quectelEC200SProductID       = 0x6002
	quectelEC100YProductID       = 0x6001
	quectelEC801EProductID       = 0x0903
	quectelEG915QProductID       = 0x6007
	quectel0125SerialPortCount   = 4
	quectel0125ATSerialPortIndex = 2
	unknownUSBInterfaceNumber    = -1
	unknownUSBSerialPortIndex    = -1
)

type usbDeviceIdentity struct {
	vendorID  uint16
	productID uint16
}

// usbATOnlyLayout describes USB compositions managed only through an AT port.
type usbATOnlyLayout struct {
	profile            modem.ATProfile
	preferredInterface int
	alternateInterface int
}

func (id usbDeviceIdentity) atOnlyLayout() (usbATOnlyLayout, bool) {
	if id.vendorID == cmIoTVendorID && id.productID == ml307cProductID {
		return usbATOnlyLayout{
			profile:            modem.ATProfileML307C,
			preferredInterface: 2,
			alternateInterface: 3,
		}, true
	}
	return usbATOnlyLayout{}, false
}

type usbATPortRule struct {
	interfaceNumber int
	serialPortCount int
	serialPortIndex int
}

func usbATPortRuleFor(identity usbDeviceIdentity, mode string) (usbATPortRule, bool) {
	if layout, ok := identity.atOnlyLayout(); ok {
		return usbInterfaceATPortRule(layout.preferredInterface), true
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if identity.vendorID == qualcommVendorID && mode == "qmi" {
		return usbInterfaceATPortRule(2), true
	}
	if identity.vendorID != quectelVendorID {
		return usbATPortRule{}, false
	}
	if identity.productID == quectel0125ProductID {
		return quectel0125ATPortRule(mode), true
	}
	if !supportsStaticQuectelATInterface(mode) {
		return usbATPortRule{}, false
	}

	// Quectel QConnectManager publishes these device-local interface numbers
	// for ECM/RNDIS/NCM. QMI entries retain this project's existing hints.
	switch identity.productID {
	case quectelEC200UProductID, quectelEC200DProductID, quectelRG801HProductID:
		return usbInterfaceATPortRule(2), true
	case quectelRG500UProductID:
		return usbInterfaceATPortRule(4), true
	case quectelEC200TProductID, quectelEC200AProductID, quectelEC200SProductID,
		quectelEC100YProductID:
		return usbInterfaceATPortRule(3), true
	case quectelEC801EProductID:
		if mode == "qmi" {
			return usbInterfaceATPortRule(2), true
		}
		if mode == "ecm" || mode == "rndis" {
			return usbInterfaceATPortRule(3), true
		}
		return usbATPortRule{}, false
	case quectelEG915QProductID:
		if mode == "rndis" {
			return usbInterfaceATPortRule(5), true
		}
		return usbInterfaceATPortRule(3), true
	default:
		if mode == "qmi" {
			return usbInterfaceATPortRule(2), true
		}
		return usbATPortRule{}, false
	}
}

func usbInterfaceATPortRule(interfaceNumber int) usbATPortRule {
	return usbATPortRule{
		interfaceNumber: interfaceNumber,
		serialPortIndex: unknownUSBSerialPortIndex,
	}
}

func quectel0125ATPortRule(mode string) usbATPortRule {
	interfaceNumber := unknownUSBInterfaceNumber
	if mode == "qmi" {
		interfaceNumber = 2
	}
	return usbATPortRule{
		interfaceNumber: interfaceNumber,
		serialPortCount: quectel0125SerialPortCount,
		serialPortIndex: quectel0125ATSerialPortIndex,
	}
}

func supportsStaticQuectelATInterface(mode string) bool {
	switch mode {
	case "qmi", "ecm", "rndis", "ncm":
		return true
	default:
		return false
	}
}

func discoverATPortsForUSBDevice(usbPath string, identity usbDeviceIdentity, mode string) (ports []string, bestPort, imei string) {
	portScan := scanATPortsForUSBDevice(usbPath)
	if layout, ok := identity.atOnlyLayout(); ok {
		var atOnly usbATPortScan
		for _, port := range portScan.interfaceOrdered {
			if port.interfaceNumber == layout.preferredInterface || port.interfaceNumber == layout.alternateInterface {
				atOnly.interfaceOrdered = append(atOnly.interfaceOrdered, port)
				atOnly.candidates = append(atOnly.candidates, port.path)
			}
		}
		portScan = atOnly
	}
	bestPort, imei = selectBestATPortForUSBDevice(identity, mode, portScan)
	return portScan.candidates, bestPort, imei
}

// selectBestATPortForUSBDevice applies only verified model/composition rules.
// Unknown layouts retain the legacy candidate ordering for dynamic probing.
func selectBestATPortForUSBDevice(identity usbDeviceIdentity, mode string, scan usbATPortScan) (bestPort, imei string) {
	if !usbATPortScanIsConsistent(scan) {
		return selectBestATPort(scan.candidates)
	}
	rule, ok := usbATPortRuleFor(identity, mode)
	if !ok {
		return selectBestATPort(scan.candidates)
	}

	if rule.interfaceNumber != unknownUSBInterfaceNumber {
		for _, port := range scan.interfaceOrdered {
			if port.interfaceNumber == rule.interfaceNumber {
				return port.path, ""
			}
		}
	}
	if rule.serialPortCount == len(scan.interfaceOrdered) && rule.serialPortIndex >= 0 {
		return scan.interfaceOrdered[rule.serialPortIndex].path, ""
	}
	return selectBestATPort(scan.candidates)
}
