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
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
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

type success struct {
	Url     string
	QrImage string
}

func main() {
	var err error
	client, err = storage.NewClient(ctx)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	go openBrowser()
	http.HandleFunc("/", serveRoot)
	http.HandleFunc("/upload", upload)
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
	reload := `
	<script>location.reload()</script>
	`
	var d data

	id := r.URL.Query().Get("roastId")

	dataObj := client.Bucket(bucket).Object(fmt.Sprintf("%s/data.json", id))
	dataReader, err := dataObj.NewReader(ctx)
	defer dataReader.Close()
	if err != nil {
		fmt.Fprint(w, reload)
		return
	}
	dataReader.Attrs.CacheControl = "no-cache, no-store, max-age=0"
	raw, err := io.ReadAll(dataReader)
	if err != nil {
		fmt.Fprint(w, reload)
		return
	}

	err = json.Unmarshal(raw, &d)
	if err != nil {
		fmt.Fprint(w, reload)
		return
	}

	tmpl, _ := template.New("").Parse(inputs + templates)
	tmpl.Execute(w, d)
}

func upload(w http.ResponseWriter, r *http.Request) {
	var id string
	err := r.ParseMultipartForm(100 * 1024 * 1024) // 100MB
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
	var dataFile io.Reader
	dataFile = bytes.NewReader(jsonData)

	// upload JSON data
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

	// Upload beans image
	var beansImageFile io.Reader
	beansImageFile, _, err = r.FormFile("beansImage")
	if err != nil {
		beansImageFile = bytes.NewReader(defaultImg)
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

	// Upload roast data image
	var roastDataImageFile io.Reader
	roastDataImageFile, _, err = r.FormFile("roastDataImage")
	if err != nil {
		roastDataImageFile = bytes.NewReader(defaultImg)
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
	var png []byte
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}

// take the string values, convert them to floats
// and return the weight loss and roast level
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

	// for now this doesn't seem very accurate for my roaster,
	// but I might revisit later
	// https://library.sweetmarias.com/how-to-calculate-weight-loss-in-coffee-roasting/

	//var degreeOfRoast string
	//switch {
	//case weightLoss <= 11.5:
	//	degreeOfRoast = "1st crack (extremely light)"
	//case weightLoss >= 11.5 && weightLoss < 12.7:
	//	degreeOfRoast = "City-"
	//case weightLoss >= 12.7 && weightLoss < 13.3:
	//	degreeOfRoast = "City"
	//case weightLoss >= 13.3 && weightLoss < 14.5:
	//	degreeOfRoast = "City+"
	//case weightLoss >= 14.5 && weightLoss < 15.1:
	//	degreeOfRoast = "Full City"
	//case weightLoss >= 15.1 && weightLoss < 15.6:
	//	degreeOfRoast = "Full City+"
	//case weightLoss >= 15.6 && weightLoss < 16.6:
	//	degreeOfRoast = "French"
	//case weightLoss >= 16.6:
	//	degreeOfRoast = "Burnt 🙃️"
	//default:
	//	degreeOfRoast = ""
	//}

	//return fmt.Sprintf("%.2f%% - %s", weightLoss, degreeOfRoast)
	return fmt.Sprintf("%.2f%%", weightLoss)
}

func openBrowser() {
	// wait a second for the web server to start up
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
