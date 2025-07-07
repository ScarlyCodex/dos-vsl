package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

var (
	// Flags
	requestFile    string
	totalRequests  int
	maxConcurrency int
	timeoutMs      int
	tfMode         bool
	rudyMode       bool
	increaser      bool
	modifiersStr   string
	wordlistFile   string
	outputFile     string
	maxErrorRate   float64 = 0.2
	maxErrorCount  int     = 500

	// Estado global
	sentCount         int32
	errorCount        int32
	abortSignal       int32
	status200Count    int32
	status429Count    int32
	status500Count    int32
	firstFailureIndex int32 = -1

	// Para análisis de latencias
	responseTimes []time.Duration
	rtMutex       sync.Mutex
)

func main() {
	// --- Parseo de flags ---
	flag.StringVar(&requestFile, "r", "", "Path to HTTP raw request file (Burp format)")
	flag.IntVar(&totalRequests, "n", 50000, "Total number of requests")
	flag.IntVar(&maxConcurrency, "c", 200, "Maximum concurrency")
	flag.IntVar(&timeoutMs, "t", 0, "Request timeout en ms (opcional)")
	flag.BoolVar(&tfMode, "tf", false, "Modo traditional flood")
	flag.BoolVar(&rudyMode, "rudy", false, "Modo throttle test (modifica campos)")
	flag.BoolVar(&increaser, "increaser", false, "Usa incremento en lugar de wordlist")
	flag.StringVar(&modifiersStr, "modifier", "", "Campos a modificar, ej: token|name|date")
	flag.StringVar(&wordlistFile, "w", "", "Path a wordlist para throttle tests")
	flag.StringVar(&outputFile, "o", "", "Archivo donde guardar todo el log")
	flag.Parse()

	// Validaciones básicas
	if requestFile == "" {
		fmt.Println(colorRed + "Error:" + colorReset + " necesitas -r")
		os.Exit(1)
	}
	if !tfMode && !rudyMode {
		tfMode = true // por defecto traditional flood
	}
	if rudyMode && modifiersStr == "" {
		fmt.Println(colorRed + "Error:" + colorReset + " --modifier es obligatorio con --rudy")
		os.Exit(1)
	}
	if rudyMode && !increaser && wordlistFile == "" {
		fmt.Println(colorRed + "Error:" + colorReset + " debes usar --increaser o -w")
		os.Exit(1)
	}

	// Prepara output (stdout + archivo si se pide)
	var out io.Writer = os.Stdout
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			fmt.Printf(colorRed+"Error creando %s: %v"+colorReset+"\n", outputFile, err)
			os.Exit(1)
		}
		defer f.Close()
		out = io.MultiWriter(os.Stdout, f)
	}

	// --- Parsea la petición raw ---
	fmt.Fprintln(out, colorCyan+"Parsing HTTP request…"+colorReset)
	method, url, headers, body, amzTarget, err := parseRawRequest(requestFile)
	if err != nil {
		fmt.Fprintf(out, colorRed+"❌ Falló parseo: %v"+colorReset+"\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "Request: [%s] %s\n", method, url)
	if amzTarget != "" {
		fmt.Fprintf(out, "X-Amz-Target: %s\n", amzTarget)
	}

	// Lista de modificadores y wordlist
	var modifiers []string
	if rudyMode {
		modifiers = strings.Split(modifiersStr, "|")
	}
	var wordlist []string
	if wordlistFile != "" {
		content, err := os.ReadFile(wordlistFile)
		if err != nil {
			fmt.Fprintf(out, colorRed+"Error leyendo wordlist: %v"+colorReset+"\n", err)
			os.Exit(1)
		}
		wordlist = strings.Split(strings.TrimSpace(string(content)), "\n")
	}

	// Cliente HTTP con timeout opcional
	client := &http.Client{}
	if timeoutMs > 0 {
		client.Timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	fmt.Fprintf(out, "%sIniciando %d requests (concurrency %d)%s\n",
		colorGreen, totalRequests, maxConcurrency, colorReset)

	// Canal para el spinner
	done := make(chan bool)
	go showProgress(done, out)

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	start := time.Now()

	// Ciclo principal
	for i := 0; i < totalRequests && atomic.LoadInt32(&abortSignal) == 0; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			// Construye el cuerpo (posible modificación)
			contentType := headers["Content-Type"]
			reqBody := body
			if rudyMode {
				reqBody = modifyBody(body, modifiers, wordlist, increaser, idx, contentType)
			}

			req, _ := http.NewRequest(method, url, bytes.NewReader(reqBody))
			// Cabeceras
			for k, v := range headers {
				if strings.ToLower(k) == "content-length" {
					continue
				}
				req.Header.Set(k, v)
			}
			req.Header.Set("Content-Length", strconv.Itoa(len(reqBody)))

			// Mide latencia
			t0 := time.Now()
			resp, err := client.Do(req)
			lat := time.Since(t0)

			atomic.AddInt32(&sentCount, 1)
			// Guarda latencia
			rtMutex.Lock()
			responseTimes = append(responseTimes, lat)
			rtMutex.Unlock()

			// Registra status y errores
			if err != nil {
				atomic.AddInt32(&errorCount, 1)
				atomic.CompareAndSwapInt32(&firstFailureIndex, -1, int32(idx+1))
			} else {
				defer resp.Body.Close()
				io.Copy(io.Discard, resp.Body)
				switch resp.StatusCode {
				case 200:
					atomic.AddInt32(&status200Count, 1)
				case 429:
					atomic.AddInt32(&status429Count, 1)
					atomic.AddInt32(&errorCount, 1)
					atomic.CompareAndSwapInt32(&firstFailureIndex, -1, int32(idx+1))
				default:
					if resp.StatusCode >= 500 {
						atomic.AddInt32(&status500Count, 1)
						atomic.AddInt32(&errorCount, 1)
						atomic.CompareAndSwapInt32(&firstFailureIndex, -1, int32(idx+1))
					}
				}
				// Abort si tasa de error alta
				if float64(atomic.LoadInt32(&errorCount))/float64(atomic.LoadInt32(&sentCount)) > maxErrorRate ||
					atomic.LoadInt32(&errorCount) >= int32(maxErrorCount) {
					atomic.StoreInt32(&abortSignal, 1)
				}
			}
		}(i)
	}

	wg.Wait()
	done <- true
	totalDur := time.Since(start)

	// --- Resumen final ---
	fmt.Fprintln(out, "\n"+colorCyan+"📋 Test Summary"+colorReset)
	fmt.Fprintln(out, "--------------------------------")
	fmt.Fprintf(out, "%sTotal enviados:%s   %d\n", colorGreen, colorReset, sentCount)
	fmt.Fprintf(out, "%s200 OK:%s         %d\n", colorGreen, colorReset, status200Count)
	fmt.Fprintf(out, "%s429 Too Many:%s   %d\n", colorYellow, colorReset, status429Count)
	fmt.Fprintf(out, "%s5xx Errors:%s     %d\n", colorRed, colorReset, status500Count)
	fmt.Fprintf(out, "Primera falla en request: %d\n", firstFailureIndex)
	fmt.Fprintf(out, "Duración total: %.2fs\n", totalDur.Seconds())

	// Analiza degradación
	analyzeDegradation(out)

	if atomic.LoadInt32(&abortSignal) == 1 {
		fmt.Fprintln(out, colorYellow+"⚠️  Flood aborted: alta tasa de error"+colorReset)
	} else {
		fmt.Fprintln(out, colorGreen+"✅ Flood completado. Target responsivo"+colorReset)
	}
}

// showProgress muestra un spinner con métricas en tiempo real
func showProgress(done <-chan bool, out io.Writer) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for {
		select {
		case <-done:
			fmt.Fprintf(out, "\r✓ Completed: %d requests.                   \n", sentCount)
			return
		default:
			c := atomic.LoadInt32(&sentCount)
			e := atomic.LoadInt32(&errorCount)
			p := float64(c) / float64(totalRequests) * 100
			s2 := atomic.LoadInt32(&status200Count)
			s4 := atomic.LoadInt32(&status429Count)
			s5 := atomic.LoadInt32(&status500Count)
			fmt.Fprintf(out, "\r%s Progress: %d/%d (%.1f%%) - 200:%d 429:%d 5xx:%d err:%d",
				frames[i], c, totalRequests, p, s2, s4, s5, e)
			time.Sleep(100 * time.Millisecond)
			i = (i + 1) % len(frames)
		}
	}
}

// parseRawRequest lee un archivo Burp raw y extrae método, URL, headers y body
func parseRawRequest(filePath string) (method, fullURL string, headers map[string]string, body []byte, amzTarget string, err error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	sections := strings.SplitN(string(raw), "\n\n", 2)
	if len(sections) < 1 {
		err = fmt.Errorf("invalid format: headers not found")
		return
	}
	lines := strings.Split(sections[0], "\n")
	parts := strings.Fields(lines[0])
	if len(parts) < 2 {
		err = fmt.Errorf("invalid request line")
		return
	}
	method = parts[0]
	path := parts[1]
	headers = make(map[string]string)
	var host string
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		col := strings.Index(ln, ":")
		if col < 0 {
			continue
		}
		k := strings.TrimSpace(ln[:col])
		v := strings.TrimSpace(ln[col+1:])
		if strings.ToLower(k) == "host" {
			host = v
		}
		if strings.ToLower(k) == "x-amz-target" {
			amzTarget = v
		}
		headers[k] = v
	}
	if host == "" {
		err = fmt.Errorf("missing Host header")
		return
	}
	fullURL = "https://" + host + path
	if len(sections) == 2 {
		body = []byte(sections[1])
	}
	return
}

// modifyBody soporta x-www-form-urlencoded y JSON via regex
func modifyBody(orig []byte, modifiers, wordlist []string, increaser bool, idx int, contentType string) []byte {
	bodyStr := string(orig)

	// Si es form-urlencoded
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		vals, err := url.ParseQuery(bodyStr)
		if err == nil {
			for _, key := range modifiers {
				if increaser {
					base := vals.Get(key)
					vals.Set(key, fmt.Sprintf("%s+%d", base, idx+1))
				} else if len(wordlist) > 0 {
					vals.Set(key, wordlist[idx%len(wordlist)])
				}
			}
			return []byte(vals.Encode())
		}
	}

	// Fallback JSON u otro texto plano (regex)
	for _, key := range modifiers {
		re := regexp.MustCompile(fmt.Sprintf(`"%s"\s*:\s*"([^"]*)`, regexp.QuoteMeta(key)))
		if increaser {
			match := re.FindStringSubmatch(bodyStr)
			base := ""
			if len(match) >= 2 {
				base = match[1]
			}
			newVal := fmt.Sprintf("%s+%d", base, idx+1)
			bodyStr = re.ReplaceAllString(bodyStr, fmt.Sprintf(`"%s":"%s`, key, newVal))
		} else if len(wordlist) > 0 {
			newVal := wordlist[idx%len(wordlist)]
			bodyStr = re.ReplaceAllString(bodyStr, fmt.Sprintf(`"%s":"%s`, key, newVal))
		}
	}

	return []byte(bodyStr)
}

// analyzeDegradation compara la latencia promedio del primer 10% vs último 10% de las requests
func analyzeDegradation(out io.Writer) {
    rtMutex.Lock()
    defer rtMutex.Unlock()
    n := len(responseTimes)
    if n < 2 {
        fmt.Fprintln(out, "No hay suficientes datos para análisis de degradación")
        return
    }

    // Definimos el tamaño de cada “bucket” como 10% del total
    bucket := n / 10
    if bucket < 1 {
        bucket = 1
    }

    // Sumamos las latencias del primer y último bucket
    var sumFirst, sumLast int64
    for i := 0; i < bucket && i < n; i++ {
        sumFirst += int64(responseTimes[i])
    }
    for i := n - bucket; i < n; i++ {
        sumLast += int64(responseTimes[i])
    }

    // Calculamos los promedios
    avgFirst := time.Duration(sumFirst / int64(bucket))
    avgLast  := time.Duration(sumLast  / int64(bucket))

    // Mostramos los resultados
    fmt.Fprintf(out,
        "Avg lat_first: %s%s%s  Avg lat_last: %s%s%s\n",
        colorCyan, avgFirst, colorReset,
        colorCyan, avgLast,  colorReset,
    )

    // Evaluamos si hay degradación (>2× aumento)
    if avgLast > avgFirst*2 {
        fmt.Fprintln(out, colorRed+"⚠️  Degradación significativa detectada"+colorReset)
    } else {
        fmt.Fprintln(out, colorGreen+"✅ Sin degradación significativa"+colorReset)
    }
}
