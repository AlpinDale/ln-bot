# --- build stage ---
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lnbot ./cmd/lnbot

# --- runtime stage ---
# distroless/static ships CA certificates; tzdata is embedded in the
# binary via time/tzdata, so no OS packages are needed.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /lnbot /lnbot

# SQLite database lives here; mount a volume.
VOLUME /data
WORKDIR /

ENTRYPOINT ["/lnbot", "-config", "/config.yaml"]
