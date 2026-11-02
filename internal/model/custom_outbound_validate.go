package model

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func ValidateCustomOutbounds(outbounds []OutboundConfig) error {
	return ValidateCustomOutboundsForKernel(outbounds, "", nil)
}

func ValidateCustomOutboundsForKernel(outbounds []OutboundConfig, kernelType string, additionalTags []string) error {
	if len(outbounds) == 0 {
		return nil
	}

	kernelType = strings.ToLower(strings.TrimSpace(kernelType))
	allowedProtocols := map[string]struct{}{}
	if kernelType != "" {
		support, ok := OutboundSupportMatrix()[kernelType]
		if !ok {
			return fmt.Errorf("unsupported kernel type %q", kernelType)
		}
		for _, protocol := range support.Protocols {
			allowedProtocols[strings.ToLower(protocol)] = struct{}{}
		}
	}

	seen := make(map[string]struct{}, len(outbounds))
	availableTags := make(map[string]struct{}, len(outbounds)+len(additionalTags)+2)
	availableTags["direct"] = struct{}{}
	availableTags["block"] = struct{}{}
	for _, tag := range additionalTags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			availableTags[tag] = struct{}{}
		}
	}

	for i, outbound := range outbounds {
		tag := strings.TrimSpace(outbound.Tag)
		protocol := strings.ToLower(strings.TrimSpace(outbound.Protocol))
		if tag == "" {
			return fmt.Errorf("custom_outbounds[%d].tag is required", i)
		}
		if protocol == "" {
			return fmt.Errorf("custom_outbounds[%d].protocol is required", i)
		}
		if kernelType != "" {
			if _, ok := allowedProtocols[protocol]; !ok {
				return fmt.Errorf("custom_outbounds[%d].protocol %q is not supported by kernel %q", i, outbound.Protocol, kernelType)
			}
		}
		if err := validateOutboundSettings(i, protocol, kernelType, outbound.Settings); err != nil {
			return err
		}
		key := strings.ToLower(tag)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("custom_outbounds[%d].tag duplicates %q", i, tag)
		}
		seen[key] = struct{}{}
		availableTags[key] = struct{}{}
	}

	for i, outbound := range outbounds {
		if outbound.ProxyTag == "" {
			continue
		}
		proxyTag := strings.ToLower(strings.TrimSpace(outbound.ProxyTag))
		if proxyTag == "" {
			return fmt.Errorf("custom_outbounds[%d].proxy_tag must not be blank", i)
		}
		if proxyTag == strings.ToLower(strings.TrimSpace(outbound.Tag)) {
			return fmt.Errorf("custom_outbounds[%d].proxy_tag must not reference itself", i)
		}
		if _, exists := availableTags[proxyTag]; !exists {
			return fmt.Errorf("custom_outbounds[%d].proxy_tag references unknown outbound %q", i, outbound.ProxyTag)
		}
	}

	return nil
}

// flatRequiredFields lists the flat (sing-box style) settings each
// protocol needs. It is only enforced when the settings are not in a
// native xray shape (see isNativeXraySettings): xray passes settings
// verbatim, so a vmess outbound with only "vnext" is legitimate and
// must keep working.
var flatRequiredFields = map[string][]string{
	"vmess":       {"server", "port", "uuid"},
	"vless":       {"server", "port", "uuid"},
	"trojan":      {"server", "port", "password"},
	"shadowsocks": {"server", "port", "password"},
	"socks":       {"server", "port"},
	"http":        {"server", "port"},
	"wireguard":   {"server", "port"},
	"tuic":        {"server", "port", "uuid", "password"},
	"hysteria2":   {"server", "port", "password"},
	"anytls":      {"server", "port", "password"},
}

// isNativeXraySettings reports whether settings use xray's native
// container shape for the protocol instead of flat server/port keys.
func isNativeXraySettings(protocol string, settings map[string]any) bool {
	switch protocol {
	case "vmess":
		_, ok := settings["vnext"]
		return ok
	case "vless", "trojan", "shadowsocks", "socks", "http":
		_, ok := settings["servers"]
		return ok
	case "wireguard":
		// Flat wireguard also uses "peers", but always with a server;
		// a peers-only shape is the native one.
		_, hasPeers := settings["peers"]
		_, hasServer := settings["server"]
		return hasPeers && !hasServer
	}
	return false
}

func validateOutboundSettings(index int, protocol, kernelType string, settings map[string]any) error {
	if len(settings) == 0 {
		return fmt.Errorf("custom_outbounds[%d].settings is required", index)
	}
	// "type" is reserved too: the sing-box converter copies settings
	// verbatim into the outbound object, so a settings.type would
	// silently overwrite the protocol type set from the outbound.
	for _, reservedKey := range []string{"tag", "protocol", "proxy_tag", "proxyTag", "type"} {
		if _, exists := settings[reservedKey]; exists {
			return fmt.Errorf("custom_outbounds[%d].settings.%s is reserved", index, reservedKey)
		}
	}
	for _, field := range []string{"server", "uuid", "password", "method", "cipher", "private_key", "secret_key", "privateKey", "secretKey"} {
		if value, ok := settings[field]; ok {
			if strings.TrimSpace(fmt.Sprintf("%v", value)) == "" {
				return fmt.Errorf("custom_outbounds[%d].settings.%s must not be blank", index, field)
			}
		}
	}
	if value, ok := settings["server_port"]; ok {
		if _, err := asPort(value); err != nil {
			return fmt.Errorf("custom_outbounds[%d].settings.server_port %w", index, err)
		}
	}
	if value, ok := settings["serverPort"]; ok {
		if _, err := asPort(value); err != nil {
			return fmt.Errorf("custom_outbounds[%d].settings.serverPort %w", index, err)
		}
	}
	if err := validateFlatRequiredFields(index, protocol, kernelType, settings); err != nil {
		return err
	}
	return nil
}

// validateFlatRequiredFields enforces per-protocol required fields for
// flat settings, plus shapes for server and uuid. It is skipped for
// native xray shapes (kernel xray or unknown) and for protocols with
// no entry in flatRequiredFields.
func validateFlatRequiredFields(index int, protocol, kernelType string, settings map[string]any) error {
	required, ok := flatRequiredFields[protocol]
	if !ok {
		return nil
	}
	if kernelType != "singbox" && isNativeXraySettings(protocol, settings) {
		return nil
	}
	nonBlank := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := settings[k]; ok && strings.TrimSpace(fmt.Sprintf("%v", v)) != "" {
				return true
			}
		}
		return false
	}
	for _, field := range required {
		switch field {
		case "port":
			if !nonBlank("server_port", "serverPort") {
				return fmt.Errorf("custom_outbounds[%d].settings.server_port is required for protocol %q", index, protocol)
			}
		case "uuid":
			if !nonBlank("uuid") {
				return fmt.Errorf("custom_outbounds[%d].settings.uuid is required for protocol %q", index, protocol)
			}
			if !uuidRe.MatchString(strings.TrimSpace(fmt.Sprintf("%v", settings["uuid"]))) {
				return fmt.Errorf("custom_outbounds[%d].settings.uuid is not a valid UUID", index)
			}
		default:
			if !nonBlank(field) {
				return fmt.Errorf("custom_outbounds[%d].settings.%s is required for protocol %q", index, field, protocol)
			}
		}
	}
	if protocol == "shadowsocks" && !nonBlank("method", "cipher") {
		return fmt.Errorf("custom_outbounds[%d].settings.method is required for protocol %q", index, protocol)
	}
	if protocol == "wireguard" && !nonBlank("private_key", "secret_key", "privateKey", "secretKey") {
		return fmt.Errorf("custom_outbounds[%d].settings.private_key is required for protocol %q", index, protocol)
	}
	if server, ok := settings["server"]; ok {
		host := strings.TrimSpace(fmt.Sprintf("%v", server))
		if net.ParseIP(host) == nil {
			if err := validateHostname(strings.TrimSuffix(host, ".")); err != nil {
				return fmt.Errorf("custom_outbounds[%d].settings.server %q is not a valid hostname or IP", index, host)
			}
		}
	}
	return nil
}

func asPort(value any) (int, error) {
	switch v := value.(type) {
	case int:
		if v < 1 || v > 65535 {
			return 0, fmt.Errorf("must be between 1 and 65535")
		}
		return v, nil
	case int32:
		return asPort(int(v))
	case int64:
		return asPort(int(v))
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("must be an integer")
		}
		return asPort(int(v))
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return asPort(parsed)
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}
