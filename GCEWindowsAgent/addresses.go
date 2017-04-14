//  Copyright 2017 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/compute-image-windows/logger"
	"github.com/go-ini/ini"
)

var (
	addressDisabled  = false
	addressKey       = regKeyBase + `\ForwardedIps`
	oldWSFCAddresses string
	oldWSFCEnable    bool
)

type addresses struct {
	newMetadata, oldMetadata *metadataJSON
	config                   *ini.File
}

func (a *addresses) parseWSFCAddresses() string {
	wsfcAddresses := a.config.Section("wsfc").Key("addresses").String()
	if len(wsfcAddresses) > 0 {
		return wsfcAddresses
	}
	if len(a.newMetadata.Instance.Attributes.WSFCAddresses) > 0 {
		return a.newMetadata.Instance.Attributes.WSFCAddresses
	}
	if len(a.newMetadata.Project.Attributes.WSFCAddresses) > 0 {
		return a.newMetadata.Project.Attributes.WSFCAddresses
	}

	return ""
}

func (a *addresses) parseWSFCEnable() bool {
	wsfcEnable, err := a.config.Section("wsfc").Key("enable").Bool()
	if err == nil {
		return wsfcEnable
	}
	wsfcEnable, err = strconv.ParseBool(a.newMetadata.Instance.Attributes.EnableWSFC)
	if err == nil {
		return wsfcEnable
	}
	wsfcEnable, err = strconv.ParseBool(a.newMetadata.Project.Attributes.EnableWSFC)
	if err == nil {
		return wsfcEnable
	}
	return false
}

func (a *addresses) diff() bool {
	wsfcAddresses := a.parseWSFCAddresses()
	wsfcEnable := a.parseWSFCEnable()

	diff := !reflect.DeepEqual(a.newMetadata.Instance.NetworkInterfaces, a.oldMetadata.Instance.NetworkInterfaces) ||
		wsfcEnable != oldWSFCEnable || wsfcAddresses != oldWSFCAddresses

	oldWSFCAddresses = wsfcAddresses
	oldWSFCEnable = wsfcEnable
	return diff
}

func (a *addresses) disabled() (disabled bool) {
	var err error

	defer func() {
		if disabled != addressDisabled {
			addressDisabled = disabled
			logStatus("address", disabled)
		}
	}()

	disabled, err = strconv.ParseBool(a.config.Section("addressManager").Key("disable").String())
	if err == nil {
		return disabled
	}
	disabled, err = strconv.ParseBool(a.newMetadata.Instance.Attributes.DisableAddressManager)
	if err == nil {
		return disabled
	}
	disabled, err = strconv.ParseBool(a.newMetadata.Project.Attributes.DisableAddressManager)
	if err == nil {
		return disabled
	}
	return addressDisabled
}

func compareIPs(regFwdIPs, mdFwdIPs, cfgIPs []string) (toAdd []string, toRm []string) {
	for _, mdIP := range mdFwdIPs {
		if !containsString(mdIP, cfgIPs) {
			toAdd = append(toAdd, mdIP)
		}
	}

	for _, cfgIP := range cfgIPs {
		if containsString(cfgIP, regFwdIPs) && !containsString(cfgIP, mdFwdIPs) {
			toRm = append(toRm, cfgIP)
		}
	}

	return
}

var badMAC []string

func (a *addresses) set() error {
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}

	a.applyWSFCFilter()

	for _, ni := range a.newMetadata.Instance.NetworkInterfaces {
		mac, err := net.ParseMAC(ni.Mac)
		if err != nil {
			if !containsString(ni.Mac, badMAC) {
				logger.Error(err)
				badMAC = append(badMAC, ni.Mac)
			}
			continue
		}

		regFwdIPs, err := readRegMultiString(addressKey, mac.String())
		if err != nil && err != errRegNotExist {
			logger.Error(err)
			continue
		} else if err != nil && err == errRegNotExist {
			regFwdIPs = nil
		}

		var iface net.Interface
		for _, i := range ifs {
			if i.HardwareAddr.String() == mac.String() {
				iface = i
			}
		}

		if reflect.DeepEqual(net.Interface{}, iface) {
			if !containsString(ni.Mac, badMAC) {
				logger.Errorf("no interface with mac %s exists on system", mac)
				badMAC = append(badMAC, ni.Mac)
			}
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			logger.Error(err)
			continue
		}

		var cfgIPs []string
		for _, addr := range addrs {
			cfgIPs = append(cfgIPs, strings.TrimSuffix(addr.String(), "/32"))
		}

		toAdd, toRm := compareIPs(regFwdIPs, ni.ForwardedIps, cfgIPs)
		if len(toAdd) != 0 || len(toRm) != 0 {
			logger.Infof("Changing forwarded IPs for %s from %q to %q by adding %q and removing %q.", mac, regFwdIPs, ni.ForwardedIps, toAdd, toRm)
		}

		reg := ni.ForwardedIps
		for _, ip := range toAdd {
			if err := addIPAddress(net.ParseIP(ip), net.ParseIP("255.255.255.255"), iface.Index); err != nil {
				logger.Error(err)
				for i, rIP := range reg {
					if rIP == ip {
						reg = append(regFwdIPs[:i], regFwdIPs[i+1:]...)
						break
					}
				}
			}
		}

		for _, ip := range toRm {
			if err := deleteIPAddress(net.ParseIP(ip)); err != nil {
				logger.Error(err)
				reg = append(reg, ip)
			}
		}

		if err := writeRegMultiString(addressKey, mac.String(), reg); err != nil {
			logger.Error(err)
		}
	}

	return nil
}

// Filter out forwarded ips based on WSFC (Windows Failover Cluster Settings).
// If only EnableWSFC is set, all ips in the ForwardedIps will be ignored.
// If WSFCAddresses is set (with or without EnableWSFC), only ips in the list will be filtered out.
func (a *addresses) applyWSFCFilter() {
	var wsfcAddrs []string
	for _, wsfcAddr := range strings.Split(a.newMetadata.Instance.Attributes.WSFCAddresses, ",") {
		if len(wsfcAddr) == 0 {
			continue
		}

		if net.ParseIP(wsfcAddr) == nil {
			logger.Errorln("ip address for wsfc is not in valid form", wsfcAddr)
			continue
		}

		wsfcAddrs = append(wsfcAddrs, wsfcAddr)
	}

	if len(wsfcAddrs) != 0 {
		interfaces := a.newMetadata.Instance.NetworkInterfaces
		for idx := range interfaces {
			var filteredList []string
			for _, ip := range interfaces[idx].ForwardedIps {
				if !containsString(ip, wsfcAddrs) {
					filteredList = append(filteredList, ip)
				}
			}

			interfaces[idx].ForwardedIps = filteredList
		}
	} else {
		enabled, err := strconv.ParseBool(a.newMetadata.Instance.Attributes.EnableWSFC)
		if err == nil && enabled {
			for idx := range a.newMetadata.Instance.NetworkInterfaces {
				a.newMetadata.Instance.NetworkInterfaces[idx].ForwardedIps = nil
			}
		}
	}
}