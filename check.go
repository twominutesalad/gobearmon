package gobearmon

import "bufio"
import "crypto/tls"
import "encoding/json"
import "errors"
import "fmt"
import "io"
import "io/ioutil"
import "net"
import "net/http"
import "os/exec"
import "strconv"
import "strings"
import "time"

type CheckFunc func(string) error
var checkFuncs map[string]CheckFunc

func DoCheck(check *Check) *CheckResult {
	if checkFuncs == nil {
		checkInit()
	}

	var result CheckResult
	f := checkFuncs[check.Type]

	if f == nil {
		result.Status = "offline"
		result.Message = "invalid check type: " + check.Type
	} else {
		err := f(check.Data)
		if err == nil {
			result.Status = "online"
		} else {
			result.Status = "offline"
			result.Message = err.Error()
		}
	}

	debugPrintf("performed check %s (%s); result: %v", check.Name, check.Type, result)
	return &result
}

func checkInit() {
	checkFuncs = make(map[string]CheckFunc)

	checkFuncs["http"] = func(data string) error {
		var params HttpCheckParams
		err := json.Unmarshal([]byte(data), &params)
		if err != nil {
			return fmt.Errorf("failed to decode check parameters: %v", err)
		}

		// fix parameters
		if params.Timeout == 0 {
			params.Timeout = 10
		} else if params.Timeout < 3 {
			params.Timeout = 3
		} else if params.Timeout > 30 {
			params.Timeout = 30
		}
		if params.Method == "" {
			if params.Body == "" {
				params.Method = "GET"
			} else {
				params.Method = "POST"
			}
		}

		client := &http.Client{
			Timeout: time.Duration(params.Timeout) * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: params.Insecure,
				},
			},
		}

		headers := http.Header{"User-Agent": {"gobearmon"}}
		for k, v := range params.Headers {
			headers.Set(k, v)
		}

		var body io.ReadCloser
		if len(params.Body) > 0 {
			body = ioutil.NopCloser(strings.NewReader(params.Body))
		}

		request, err := http.NewRequest(params.Method, params.Url, body)
		if err != nil {
			return fmt.Errorf("error creating HTTP request: %v", err)
		}
		request.Header = headers

		if params.Username != "" {
			request.SetBasicAuth(params.Username, params.Password)
		}

		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("error performing HTTP request: %v", err)
		}

		if params.ExpectStatus != 0 && params.ExpectStatus != response.StatusCode {
			return fmt.Errorf("status mismatch, expected %d but got %d", response.StatusCode, params.ExpectStatus)
		}

		if params.ExpectSubstring != "" {
			bytes, err := ioutil.ReadAll(response.Body)
			if err != nil {
				return fmt.Errorf("error reading HTTP response body: %v", err)
			}

			if !strings.Contains(string(bytes), params.ExpectSubstring) {
				return fmt.Errorf("expected substring [%s] was not found in the response body", params.ExpectSubstring)
			}
		}
		response.Body.Close()

		return nil
	}

	checkFuncs["tcp"] = func(data string) error {
		var params TcpCheckParams
		err := json.Unmarshal([]byte(data), &params)
		if err != nil {
			return fmt.Errorf("failed to decode check parameters: %v", err)
		}

		if params.Timeout == 0 {
			params.Timeout = 10
		} else if params.Timeout < 3 {
			params.Timeout = 3
		} else if params.Timeout > 30 {
			params.Timeout = 30
		}

		network := "tcp"
		if params.ForceIP == 4 {
			network = "tcp4"
		} else if params.ForceIP == 6 {
			network = "tcp6"
		}

		conn, err := net.DialTimeout(network, params.Address, time.Duration(params.Timeout) * time.Second)
		if err != nil {
			return fmt.Errorf("TCP connection error: %v", err)
		}
		defer conn.Close()

		if params.Expect != "" {
			if params.Payload != "" {
				_, err := conn.Write([]byte(params.Payload + "\n"))
				if err != nil {
					return fmt.Errorf("failed to send payload: %v", err)
				}
			}

			in := bufio.NewReader(conn)
			str, err := in.ReadString('\n')
			if err != nil {
				return fmt.Errorf("failed to read response: %v", err)
			} else if !strings.Contains(str, params.Expect) {
				return fmt.Errorf("response mismatch, expected [%s] but got [%s]", params.Expect, strings.TrimSpace(str))
			}
		}

		return nil
	}

	checkFuncs["icmp"] = func(data string) error {
		var params IcmpCheckParams
		err := json.Unmarshal([]byte(data), &params)
		if err != nil {
			return fmt.Errorf("failed to decode check parameters: %v", err)
		}

		command := "ping"
		if params.ForceIP == 6 {
			command = "ping6"
		}

		if strings.ContainsAny(params.Target, "$&<>!/\\\"'") {
			return errors.New("ping target contains invalid characters")
		}

		cmd := exec.Command(command, "-c", "5", "-w", "10", "--", params.Target)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("failed to run ping command: %v", err)
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			parts := strings.Split(line, " received, ")
			if len(parts) == 2 {
				percentageString := strings.Split(parts[1], "% packet loss")[0]
				percentage, err := strconv.Atoi(percentageString)
				if err != nil {
					return errors.New("failed to parse ping percent packet loss")
				} else if percentage == 100 || (percentage > 30 && params.PacketLoss) {
					return fmt.Errorf("ping: %d%% packet loss", percentage)
				} else {
					return nil
				}
			}
		}

		return errors.New("unknown ping output format")
	}

	checkFuncs["ssl_expire"] = func(data string) error {
		var params SslExpireCheckParams
		err := json.Unmarshal([]byte(data), &params)
		if err != nil {
			return fmt.Errorf("failed to decode check parameters: %v", err)
		}

		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", params.Address, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		err = conn.Handshake()
		if err != nil {
			return err
		}
		state := conn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			return errors.New("no peer certificates found")
		}
		cert := state.PeerCertificates[0]
		daysRemaining := int(cert.NotAfter.Sub(time.Now()).Hours() / 24)
		if daysRemaining <= params.Days {
			return fmt.Errorf("certificate (%s) expires in %d days", cert.Subject.CommonName, daysRemaining)
		}
		return nil
	}
}
