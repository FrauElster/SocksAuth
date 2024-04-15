package socksauth

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"time"
)

type nordServer struct {
	ID             int                 `json:"id"`
	Name           string              `json:"name"`
	Station        string              `json:"station"`
	Ipv6Station    string              `json:"ipv6_station,omitempty"`
	Hostname       string              `json:"hostname"`
	Load           int                 `json:"load"`
	Status         string              `json:"status"`
	Type           string              `json:"type"`
	Locations      []nordLocation      `json:"locations"`
	Services       []nordService       `json:"services"`
	Technologies   []nordTechnology    `json:"technologies"`
	Groups         []nordGroup         `json:"groups"`
	Specifications []nordSpecification `json:"specifications"`
	Ips            []nordIP            `json:"ips"`
}

type nordLocation struct {
	ID        int         `json:"id"`
	Latitude  float64     `json:"latitude"`
	Longitude float64     `json:"longitude"`
	Country   nordCountry `json:"country"`
}

type nordCountry struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Code string   `json:"code"`
	City nordCity `json:"city"`
}

type nordCity struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	DnsName   string  `json:"dns_name"`
	HubScore  int     `json:"hub_score"`
}

type nordService struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

type nordTechnology struct {
	ID         int           `json:"id"`
	Name       string        `json:"name"`
	Identifier string        `json:"identifier"`
	Metadata   []interface{} `json:"metadata"` // Use interface{} if the structure of metadata is not known or varies
	Pivot      nordPivot     `json:"pivot"`
}

type nordPivot struct {
	TechnologyID int    `json:"technology_id"`
	ServerID     int    `json:"server_id"`
	Status       string `json:"status"`
}

type nordGroup struct {
	ID         int      `json:"id"`
	Title      string   `json:"title"`
	Identifier string   `json:"identifier"`
	Type       nordType `json:"type"`
}

type nordType struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Identifier string `json:"identifier"`
}

type nordSpecification struct {
	ID         int         `json:"id"`
	Title      string      `json:"title"`
	Identifier string      `json:"identifier"`
	Values     []nordValue `json:"values"`
}

type nordValue struct {
	ID    int    `json:"id"`
	Value string `json:"value"`
}

type nordIP struct {
	ID       int           `json:"id"`
	ServerID int           `json:"server_id"`
	IpID     int           `json:"ip_id"`
	Type     string        `json:"type"`
	Ip       nordIPDetails `json:"ip"`
}

type nordIPDetails struct {
	ID      int    `json:"id"`
	Ip      string `json:"ip"`
	Version int    `json:"version"`
}

var servers []nordServer

// FindNordVpnServer finds a socks server from the (undocumented) NordVPN API
func FindNordVpnServer(ctx context.Context) (host string, err error) {
	// since this operation is kinda slow, we ant to use a very simple cache
	if len(servers) == 0 {
		servers, err = findNordVpnServers(ctx)
		if err != nil {
			return "", err
		}
	}

	for {
		if len(servers) == 0 {
			return "", fmt.Errorf("no socks server found")
		}

		// choose a random server
		randIdx := rand.Intn(len(servers))
		chosenAddr := servers[randIdx].Hostname + ":1080"

		// check if the server is reachable
		conn, err := net.DialTimeout("tcp", chosenAddr, time.Second)
		if err == nil {
			conn.Close()
			return chosenAddr, nil
		}

		// remove the server from the list
		servers = removeAtIndexNoOrder(servers, randIdx)
		continue
	}
}

func findNordVpnServers(ctx context.Context) ([]nordServer, error) {
	url := "https://api.nordvpn.com/v1/servers?limit=0"
	servers, err := fetchJson[[]nordServer](ctx, url)
	if err != nil {
		return nil, err
	}

	socks5Servers := make([]nordServer, 0)
	for _, server := range servers {
		if server.Status != "online" {
			continue
		}

		if server.Load > 80 {
			continue
		}

		for _, tech := range server.Technologies {
			// check if it is a SOCKS5 server
			if tech.ID == 7 {
				socks5Servers = append(socks5Servers, server)
			}
		}
	}

	return socks5Servers, nil
}

func fetchJson[T any](ctx context.Context, url string) (defaultVal T, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return defaultVal, err
	}

	// Add headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:86.0) Gecko/20100101 Firefox/86.0") // they dont have to know

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return defaultVal, err
	}

	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:86.0) Gecko/20100101 Firefox/86.0")

	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return defaultVal, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return defaultVal, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var reader io.ReadCloser
	switch res.Header.Get("Content-Encoding") {
	case "gzip":
		reader, err = gzip.NewReader(res.Body)
		defer reader.Close()
		if err != nil {
			return defaultVal, fmt.Errorf("could not decode gzip body: %w", err)
		}
	default:
		reader = res.Body
	}

	var data T
	if err := json.NewDecoder(reader).Decode(&data); err != nil {
		return defaultVal, err
	}

	return data, nil
}

// removeAtIndexNoOrder removes an element at index idx from a slice a without preserving the order of the remaining elements.
func removeAtIndexNoOrder[T any](a []T, idx int) []T {
	// Check if the index is within the range of the slice
	if idx < 0 || idx >= len(a) {
		return a
	}
	// Swap the element with the last one
	a[idx] = a[len(a)-1]
	// Return the slice excluding the last element
	return a[:len(a)-1]
}
