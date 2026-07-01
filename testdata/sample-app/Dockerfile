FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/sample-app .

FROM alpine:3.22

RUN adduser -D -H app
USER app
WORKDIR /app
COPY --from=build /out/sample-app /app/sample-app

EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD ["/app/sample-app", "healthcheck"]
CMD ["/app/sample-app", "server"]
