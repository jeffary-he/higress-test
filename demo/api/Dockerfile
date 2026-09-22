FROM golang:1.20 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/gray-demo-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gray-demo-api /gray-demo-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/gray-demo-api"]
