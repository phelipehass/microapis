package safefetch

import "net"

// reservedCIDRs cobre as faixas que net.IP não classifica sozinho e que, ainda
// assim, alcançam infraestrutura que não é do cliente. A mais crítica é a
// 169.254.169.254 (metadata da AWS/GCP/Azure) — já barrada por
// IsLinkLocalUnicast, mas mantida explícita porque é o alvo número um.
var reservedCIDRs = func() []*net.IPNet {
	blocks := []string{
		"0.0.0.0/8",       // "este host"
		"100.64.0.0/10",   // CGNAT — rede do provedor, não a internet
		"169.254.0.0/16",  // link-local, inclui a metadata da cloud
		"192.0.0.0/24",    // atribuições especiais IETF
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reservado, inclui 255.255.255.255
		"::/128",          // não especificado
		"::1/128",         // loopback v6
		"64:ff9b::/96",    // NAT64: encapsula IPv4, poderia burlar a checagem v4
		"2001:db8::/32",   // documentação
		"2002::/16",       // 6to4, mesmo risco de encapsulamento
	}
	nets := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		if _, n, err := net.ParseCIDR(b); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// isDisallowedIP responde se o IP pertence a algo que não seja a internet
// pública. Falha fechado: IP inválido é bloqueado.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Normaliza ::ffff:127.0.0.1 para 127.0.0.1, senão as checagens v4 passam batido.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}

	for _, n := range reservedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
