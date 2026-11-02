package model

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

func ValidateCustomRouteRules(rules []CustomRouteRule, kernelType string, availableTags map[string]struct{}) error {
	if len(rules) == 0 {
		return nil
	}

	kernelType = strings.ToLower(strings.TrimSpace(kernelType))
	matcherSupport := map[string]struct{}{}
	actionSupport := map[string]struct{}{}
	if kernelType != "" {
		support, ok := RouteSupportMatrix()[kernelType]
		if !ok {
			return fmt.Errorf("unsupported kernel type %q", kernelType)
		}
		for _, matcher := range support.Matchers {
			matcherSupport[strings.ToLower(matcher)] = struct{}{}
		}
		for _, action := range support.Actions {
			actionSupport[strings.ToLower(action)] = struct{}{}
		}
	}

	for i, rule := range rules {
		if rule.Disabled {
			continue
		}
		if !hasRouteMatch(rule.Match) {
			return fmt.Errorf("custom_route_rules[%d].match is required", i)
		}
		if err := ensureRouteMatcherSupported(i, kernelType, matcherSupport, rule.Match); err != nil {
			return err
		}
		if err := validatePortSpecs(rule.Match.Ports, fmt.Sprintf("custom_route_rules[%d].match.ports", i)); err != nil {
			return err
		}
		if err := validatePortSpecs(rule.Match.SourcePorts, fmt.Sprintf("custom_route_rules[%d].match.source_ports", i)); err != nil {
			return err
		}
		if err := validateNetworks(rule.Match.Networks, fmt.Sprintf("custom_route_rules[%d].match.networks", i)); err != nil {
			return err
		}
		if err := validateCIDRList(rule.Match.IPCIDRs, fmt.Sprintf("custom_route_rules[%d].match.ip_cidrs", i)); err != nil {
			return err
		}
		if err := validateCIDRList(rule.Match.SourceCIDRs, fmt.Sprintf("custom_route_rules[%d].match.source_cidrs", i)); err != nil {
			return err
		}
		if err := validateDomainList(rule.Match.Domains, fmt.Sprintf("custom_route_rules[%d].match.domains", i), kernelType); err != nil {
			return err
		}
		if err := validateDomainList(rule.Match.DomainSuffixes, fmt.Sprintf("custom_route_rules[%d].match.domain_suffixes", i), kernelType); err != nil {
			return err
		}

		actionType := strings.ToLower(strings.TrimSpace(rule.Action.Type))
		if actionType == "" {
			return fmt.Errorf("custom_route_rules[%d].action.type is required", i)
		}
		if kernelType != "" {
			if _, ok := actionSupport[actionType]; !ok {
				return fmt.Errorf("custom_route_rules[%d].action.type %q is not supported by kernel %q", i, rule.Action.Type, kernelType)
			}
		}
		target := strings.TrimSpace(rule.Action.Target)
		switch actionType {
		case "route":
			if target == "" {
				return fmt.Errorf("custom_route_rules[%d].action.target is required when action.type is route", i)
			}
			if availableTags != nil {
				if _, ok := availableTags[strings.ToLower(target)]; !ok {
					return fmt.Errorf("custom_route_rules[%d].action.target references unknown outbound %q", i, rule.Action.Target)
				}
			}
		case "direct", "block":
			if target != "" {
				return fmt.Errorf("custom_route_rules[%d].action.target is only allowed when action.type is route", i)
			}
		default:
			return fmt.Errorf("custom_route_rules[%d].action.type %q is not supported", i, rule.Action.Type)
		}
	}

	return nil
}

func hasRouteMatch(match RouteMatch) bool {
	for _, values := range [][]string{
		match.Domains,
		match.DomainSuffixes,
		match.IPCIDRs,
		match.Ports,
		match.Networks,
		match.SourceCIDRs,
		match.SourcePorts,
	} {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func ensureRouteMatcherSupported(index int, kernelType string, matcherSupport map[string]struct{}, match RouteMatch) error {
	if kernelType == "" {
		return nil
	}
	checks := map[string][]string{
		"domains":         match.Domains,
		"domain_suffixes": match.DomainSuffixes,
		"ip_cidrs":        match.IPCIDRs,
		"ports":           match.Ports,
		"networks":        match.Networks,
		"source_cidrs":    match.SourceCIDRs,
		"source_ports":    match.SourcePorts,
	}
	for matcher, values := range checks {
		if !hasAnyNonBlank(values) {
			continue
		}
		if _, ok := matcherSupport[matcher]; !ok {
			return fmt.Errorf("custom_route_rules[%d].match.%s is not supported by kernel %q", index, matcher, kernelType)
		}
	}
	return nil
}

func hasAnyNonBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// validateCIDRList accepts CIDR notation or a bare IP, which both
// kernels accept in their IP match fields. Anything else used to pass
// validation and then silently match nothing (or fail kernel startup).
func validateCIDRList(values []string, field string) error {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(value); err == nil {
			continue
		}
		if net.ParseIP(value) != nil {
			continue
		}
		return fmt.Errorf("%s contains invalid IP or CIDR %q", field, value)
	}
	return nil
}

// validateDomainList checks domain matchers. Bare hostnames are valid
// for both kernels. xray-style prefixed forms (domain:, full:,
// keyword:, ext:) are passed through verbatim to xray, so they are
// accepted for xray (and for an unknown kernel), with regexp: values
// checked for compilability. For sing-box they are rejected: sing-box
// treats them as literal domains that never match. Wildcards, spaces,
// and malformed labels are rejected for both kernels.
func validateDomainList(values []string, field string, kernelType string) error {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if prefix, rest, ok := strings.Cut(value, ":"); ok {
			if kernelType == "singbox" {
				return fmt.Errorf("%s contains xray-style domain prefix %q, which sing-box treats as a literal domain that never matches", field, value)
			}
			switch strings.ToLower(prefix) {
			case "domain", "full", "keyword", "ext":
				if strings.TrimSpace(rest) == "" {
					return fmt.Errorf("%s contains invalid domain %q", field, value)
				}
				continue
			case "regexp":
				if _, err := regexp.Compile(rest); err != nil {
					return fmt.Errorf("%s contains invalid regexp domain %q: %v", field, value, err)
				}
				continue
			}
			// A colon in any other position is not a valid hostname.
			return fmt.Errorf("%s contains invalid domain %q", field, value)
		}
		if err := validateHostname(strings.TrimPrefix(value, ".")); err != nil {
			return fmt.Errorf("%s contains invalid domain %q", field, value)
		}
	}
	return nil
}

func validateHostname(host string) error {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return fmt.Errorf("empty or too long hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return fmt.Errorf("empty or too long label")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label starts or ends with hyphen")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("invalid character in label")
			}
		}
	}
	return nil
}

func validateNetworks(values []string, field string) error {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if value != "tcp" && value != "udp" {
			return fmt.Errorf("%s contains unsupported network %q", field, value)
		}
	}
	return nil
}

func validatePortSpecs(values []string, field string) error {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, "-") || strings.Contains(value, ":") {
			delimiter := "-"
			if strings.Contains(value, ":") {
				delimiter = ":"
			}
			parts := strings.Split(value, delimiter)
			if len(parts) != 2 {
				return fmt.Errorf("%s contains invalid port range %q", field, value)
			}
			start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil || start < 1 || start > 65535 {
				return fmt.Errorf("%s contains invalid port range %q", field, value)
			}
			end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || end < 1 || end > 65535 || end < start {
				return fmt.Errorf("%s contains invalid port range %q", field, value)
			}
			continue
		}
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s contains invalid port %q", field, value)
		}
	}
	return nil
}
