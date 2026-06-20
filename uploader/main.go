package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

//go:embed static/index.html
var index string

//go:embed static/tmpl.html
var templates string

//go:embed static/inputs.html
var inputs string

//go:embed static/default.png
var defaultImg []byte

//go:embed static/success.html
var successHtml string

//go:embed static/shutdown.html
var shutdownHtml string

//go:embed static/edit.html
var editHtml string

var port string = ":5000"
var client *storage.Client
var ctx context.Context = context.Background()
var bucket string = "aro-coffee"
var baseUrl string = "https://aro.coffee/"

var sheetsConfig = struct {
	ID   string
	Name string
}{
	ID:   "1GecWtEQMF6TjGsx7rKex3FZaETM9HPNo4Cv1mS2D8_w",
	Name: "Inventory",
}

var googleSheetsService *sheets.Service
var sheetsInitErr error

func getSheetsService() (*sheets.Service, error) {
	if googleSheetsService != nil {
		return googleSheetsService, nil
	}

	sheetsSvc, err := sheets.NewService(ctx, option.WithScopes("https://www.googleapis.com/auth/spreadsheets.readonly"))
	if err != nil {
		sheetsInitErr = fmt.Errorf("sheets init failed: %w", err)
		return nil, sheetsInitErr
	}

	googleSheetsService = sheetsSvc
	return sheetsSvc, nil
}

type data struct {
	BeanName          string `json:"beanName"`
	TasteNotes        string `json:"tasteNotes"`
	GreenBeanWeight   string `json:"greenBeanWeight"`
	RoastedBeanWeight string `json:"roastedBeanWeight"`
	PurchaseUrl       string `json:"purchaseUrl"`
	UploadTime        string `json:"uploadTime"`
	Id                string `json:"id"`
	WeightLoss        string `json:"weightLoss"`
	Roastnotes        string `json:"roastNotes,omitempty"`
	Error             string `json:"-"`
}

type beansResponse struct {
	Beans []string `json:"beans"`
}

type beanRow struct {
	BeanName    string `json:"beanName"`
	PurchaseUrl string `json:"purchaseUrl"`
	TasteNotes  string `json:"tasteNotes"`
}

type success struct {
	Url     string
	QrImage string
}

type defaultFile struct {
	*bytes.Reader
}

func (d defaultFile) Close() error      { return nil }
func (d defaultFile) Size() int64       { return int64(len(defaultImg)) }
func (d defaultFile) Seek(offset int64, whence int) (int64, error) {
	return d.Reader.Seek(offset, whence)
}

var defaultFileInstance = defaultFile{bytes.NewReader(defaultImg)}

func main() {
	var err error
	client, err = storage.NewClient(ctx)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	go func() {
		svc, e := getSheetsService()
		if e != nil {
			fmt.Println("Sheets init warning:", e)
			return
		}
		_, err := svc.Spreadsheets.Get(sheetsConfig.ID).Do()
		if err != nil {
			fmt.Printf("Sheets access check failed: %v\n", err)
			fmt.Println("Make sure gcloud auth application-default login has been run")
		} else {
			fmt.Println("Sheets connected to:", sheetsConfig.Name)
		}
		_ = svc
	}()

	go openBrowser()
	http.HandleFunc("/", serveRoot)
	http.HandleFunc("/upload", upload)
	http.HandleFunc("/beans", beansHandler)
	http.HandleFunc("/beans/fill", beansFill)
	http.HandleFunc("/shutdown", shutdown)
	http.HandleFunc("/edit", edit)
	http.HandleFunc("/getData", getData)

	fmt.Println("Starting server on", port)
	http.ListenAndServe(port, nil)
}

func serveRoot(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.New("").Parse(index + templates)
	tmpl.Execute(w, nil)
}

func shutdown(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, shutdownHtml)
	go func() {
		fmt.Println("[!] Shutting down server...")
		os.Exit(0)
	}()
}

func edit(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.New("").Parse(editHtml + templates)
	tmpl.Execute(w, nil)
}

func getData(w http.ResponseWriter, r *http.Request) {
	var d data

	id := r.URL.Query().Get("roastId")

	dataObj := client.Bucket(bucket).Object(fmt.Sprintf("%s/data.json", id))
	dataReader, err := dataObj.NewReader(ctx)
	if err != nil {
		fmt.Fprint(w, "reload-page")
		return
	}
	defer dataReader.Close()

	raw, err := io.ReadAll(dataReader)
	if err != nil {
		fmt.Fprint(w, "reload-page")
		return
	}

	if err = json.Unmarshal(raw, &d); err != nil {
		fmt.Fprint(w, "reload-page")
		return
	}

	tmpl, _ := template.New("").Parse(inputs + templates)
	tmpl.Execute(w, d)
}


// beansHandler handles GET /beans queries.
// It fetches non-archived bean names from the inventory Google Sheet
// and optionally filters them by a search query parameter.
func beansHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Only allow GET requests
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Parse optional case-insensitive search query from URL
	query := strings.ToLower(r.URL.Query().Get("query"))

	// Lazily initialize and obtain the Google Sheets service
	svc, err := getSheetsService()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch beans from column A (bean names) and column D (archived flag)
	br, err := svc.Spreadsheets.Values.BatchGet(sheetsConfig.ID).Ranges(
		fmt.Sprintf("%s!A:A", sheetsConfig.Name),
		fmt.Sprintf("%s!D:D", sheetsConfig.Name),
	).Do()
	if err != nil {
		http.Error(w, fmt.Sprintf("sheets read error: %v", err), http.StatusInternalServerError)
		return
	}

	// Flatten the sheet columns into Go slices
	var colA, colD []string
	if br.ValueRanges != nil {
		if len(br.ValueRanges) > 0 {
			colA = flattenRange(br.ValueRanges[0])
		}
		if len(br.ValueRanges) > 1 {
			colD = flattenRange(br.ValueRanges[1])
		}
	}

	// Build list of beans, filtering out empty rows, headers, and archived entries
	var beans []string
	for i, name := range colA {
		name = strings.TrimSpace(name)
		// Skip empty rows and the "Bean Name" header row
		if name == "" || strings.EqualFold(name, "Bean Name") {
			continue
		}
		// Only include beans where column D (archived) is FALSE
		if !isFalse(colD, i) {
			continue
		}
		// Apply optional case-insensitive search filter
		if query == "" || strings.Contains(strings.ToLower(name), query) {
			beans = append(beans, name)
		}
	}

	json.NewEncoder(w).Encode(beansResponse{Beans: beans})
}

// beansFill handles POST /beans/fill.
// It searches the Inventory Google Sheet for a bean matching the name
// in the request body, then returns an object containing the bean's name,
// purchase URL (column B), and taste notes (column E). Archived beans are
// excluded. Returns 404 if no matching bean is found.
func beansFill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BeanName string `json:"beanName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	svc, err := getSheetsService()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch columns A (bean name), B (purchase URL), D (archived flag), E (taste notes)
	br, err := svc.Spreadsheets.Values.BatchGet(sheetsConfig.ID).Ranges(
		fmt.Sprintf("%s!A:A", sheetsConfig.Name),
		fmt.Sprintf("%s!B:B", sheetsConfig.Name),
		fmt.Sprintf("%s!D:D", sheetsConfig.Name),
		fmt.Sprintf("%s!E:E", sheetsConfig.Name),
	).Do()
	if err != nil {
		http.Error(w, fmt.Sprintf("sheets read error: %v", err), http.StatusInternalServerError)
		return
	}

	var colA, colB, colD, colE []string
	if br.ValueRanges != nil {
		if len(br.ValueRanges) > 0 {
			colA = flattenRange(br.ValueRanges[0])
		}
		if len(br.ValueRanges) > 1 {
			colB = flattenRange(br.ValueRanges[1])
		}
		if len(br.ValueRanges) > 2 {
			colD = flattenRange(br.ValueRanges[2])
		}
		if len(br.ValueRanges) > 3 {
			colE = flattenRange(br.ValueRanges[3])
		}
	}

	for i, name := range colA {
		name = strings.TrimSpace(name)
		// Skip empty rows and the header row
		if name == "" || strings.EqualFold(name, "Bean Name") {
			continue
		}
		// Skip archived beans (D column must be FALSE)
		if !isFalse(colD, i) {
			continue
		}
		// Found the matching bean (case-insensitive)
		if strings.EqualFold(name, req.BeanName) {
			buyUrl := ""
			taste := ""
			if i < len(colB) && colB[i] != "" {
				buyUrl = strings.TrimSpace(colB[i])
			}
			if i < len(colE) && colE[i] != "" {
				taste = strings.TrimSpace(colE[i])
			}
			json.NewEncoder(w).Encode(&beanRow{
				BeanName:    name,
				PurchaseUrl: buyUrl,
				TasteNotes:  taste,
			})
			return
		}
	}

	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]string{"error": "bean not found"})
}

func flattenRange(sr *sheets.ValueRange) []string {
	if sr == nil || sr.Values == nil {
		return nil
	}
	var result []string
	for _, row := range sr.Values {
		if len(row) > 0 {
			result = append(result, fmt.Sprintf("%v", row[0]))
		}
	}
	return result
}

func isFalse(col []string, i int) bool {
	if i >= len(col) {
		return false
	}
	return strings.EqualFold(col[i], "false")
}

func upload(w http.ResponseWriter, r *http.Request) {
	var id string
	err := r.ParseMultipartForm(100 * 1024 * 1024)
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, "Upload too large")
	}

	now := time.Now()
	var d data
	d.BeanName = r.FormValue("beanName")
	d.TasteNotes = r.FormValue("tasteNotes")
	d.GreenBeanWeight = r.FormValue("greenBeanWeight")
	d.RoastedBeanWeight = r.FormValue("roastBeanWeight")
	d.PurchaseUrl = r.FormValue("purchaseUrl")
	d.Roastnotes = r.FormValue("roastNotes")
	d.UploadTime = now.Format(time.RFC822)
	d.WeightLoss = calculateWeightLoss(d.GreenBeanWeight, d.RoastedBeanWeight)

	formId := r.FormValue("id")
	if formId == "" {
		id = getShortUuid()
	} else {
		id = formId
	}
	d.Id = id
	fmt.Println("ID:", id)

	jsonData, err := json.Marshal(d)
	if err != nil {
		tmpl, _ := template.New("").Parse(inputs + templates)
		tmpl.Execute(w, data{Error: err.Error()})
		return
	}
	dataFile := bytes.NewReader(jsonData)

	dataObj := client.Bucket(bucket).Object(fmt.Sprintf("%s/data.json", id))
	dataWriter := dataObj.NewWriter(ctx)

	_, err = io.Copy(dataWriter, dataFile)
	if err != nil {
		tmpl, _ := template.New("").Parse(inputs + templates)
		tmpl.Execute(w, data{Error: err.Error()})
		return
	}
	err = dataWriter.Close()
	if err != nil {
		tmpl, _ := template.New("").Parse(inputs + templates)
		tmpl.Execute(w, data{Error: err.Error()})
		return
	}

	{
		var beansImageFile multipart.File
		beansImageFile, _, err = r.FormFile("beansImage")
		if err != nil {
			beansImageFile = defaultFileInstance
		}
		beansImgObj := client.Bucket(bucket).Object(fmt.Sprintf("%s/beans-image", id))
		beansImageWriter := beansImgObj.NewWriter(ctx)
		_, err = io.Copy(beansImageWriter, beansImageFile)
		if err != nil {
			tmpl, _ := template.New("").Parse(inputs + templates)
			tmpl.Execute(w, data{Error: err.Error()})
			return
		}
		err = beansImageWriter.Close()
		if err != nil {
			tmpl, _ := template.New("").Parse(inputs + templates)
			tmpl.Execute(w, data{Error: err.Error()})
			return
		}
	}

	{
		var roastDataImageFile multipart.File
		roastDataImageFile, _, err = r.FormFile("roastDataImage")
		if err != nil {
			roastDataImageFile = defaultFileInstance
		}
		roastDataObj := client.Bucket(bucket).Object(fmt.Sprintf("%s/roast-data-image", id))
		roastDataWriter := roastDataObj.NewWriter(ctx)
		_, err = io.Copy(roastDataWriter, roastDataImageFile)
		if err != nil {
			tmpl, _ := template.New("").Parse(inputs + templates)
			tmpl.Execute(w, data{Error: err.Error()})
			return
		}
		err = roastDataWriter.Close()
		if err != nil {
			tmpl, _ := template.New("").Parse(inputs + templates)
			tmpl.Execute(w, data{Error: err.Error()})
			return
		}
	}

	fullUrl := fmt.Sprintf("%s#%s", baseUrl, id)
	imageString, err := QrBase64String(fullUrl)
	if err != nil {
		tmpl, _ := template.New("").Parse(inputs + templates)
		tmpl.Execute(w, data{Error: err.Error()})
		return
	}
	s := success{
		Url:     fullUrl,
		QrImage: imageString,
	}
	tmpl, _ := template.New("").Parse(successHtml)
	tmpl.Execute(w, s)
}

func getShortUuid() string {
	full := uuid.New()
	return full.String()[0:8]
}

func QrBase64String(url string) (string, error) {
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}

func calculateWeightLoss(greenWeight, roastWeight string) string {
	gw, err := strconv.ParseFloat(greenWeight, 64)
	if err != nil {
		return "unknown"
	}
	rw, err := strconv.ParseFloat(roastWeight, 64)
	if err != nil {
		return "unknown"
	}
	weightLoss := (gw - rw) / gw * 100
	return fmt.Sprintf("%.2f%%", weightLoss)
}

func openBrowser() {
	time.Sleep(1 * time.Second)

	osName := runtime.GOOS
	if osName == "darwin" {
		cmd := exec.Command("open", "http://localhost:5000/")
		cmd.CombinedOutput()
	}
	if osName == "linux" {
		cmd := exec.Command("xdg-open", "http://localhost:5000/")
		cmd.CombinedOutput()
	}
}
