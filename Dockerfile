FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tossinvest-exporter .

FROM alpine:3.22

RUN adduser -D -H -u 65532 exporter
WORKDIR /app

COPY --from=build /out/tossinvest-exporter /usr/local/bin/tossinvest-exporter
COPY symbols*.txt ./

ENV TOSSINVEST_LISTEN_ADDR=:9108
ENV TOSSINVEST_SYMBOLS_FILE=/app/symbols-all-stocks.txt

USER exporter
EXPOSE 9108

ENTRYPOINT ["/usr/local/bin/tossinvest-exporter"]
