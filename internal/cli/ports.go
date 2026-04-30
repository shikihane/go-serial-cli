package cli

type VirtualPortPair struct {
	Public string
	Hub    string
}

func portPairs(publicPorts []string, hubPorts []string) []VirtualPortPair {
	pairs := make([]VirtualPortPair, 0, len(publicPorts))
	for i, public := range publicPorts {
		hub := ""
		if i < len(hubPorts) {
			hub = hubPorts[i]
		}
		pairs = append(pairs, VirtualPortPair{Public: public, Hub: hub})
	}
	return pairs
}
