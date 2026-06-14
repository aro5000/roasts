.PHONY: uploader web

uploader:
	cd uploader && go run main.go

web:
	( if [ "$(uname -s)" = "Darwin" ]; then open http://localhost:8000; else xdg-open http://localhost:8000; fi )
	python -m http.server -d public/
