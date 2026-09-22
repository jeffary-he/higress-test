FROM golang:1.20 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/gray-demo-api .

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /out/gray-demo-api /gray-demo-api
USER app
EXPOSE 8080
ENTRYPOINT ["/gray-demo-api"]
