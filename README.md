# Roasts

A weekend hack that I'm going to use for sharing information on my coffee roasts (this probably isn't for you).

Yea yea yea I know I am pretty bad at frontends. If you feel so inclined to improve this situation, feel free to open a PR.

The only reason I even made this repo public is because I wanted a cheap (free) way to host the public site on GitHub Pages 🤷️

## Setup
- `public` - the public website [aro.coffee](https://aro.coffee/)
- `uploader` - a tool for me to quickly/easily upload my roast data. `cd uploader && go build`

## Authenticate to Sheets

In order to authenticate to Google Sheets the best way I've found is by using a service account. Just give the service account email address access to the Google Sheet and authenticate with gcloud like the following:

```bash
gcloud auth application-default login \
--impersonate-service-account=<service account email> \
--scopes="https://www.googleapis.com/auth/cloud-platform,https://www.googleapis.com/auth/spreadsheets"
```