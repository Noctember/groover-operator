FROM golang:alpine as builder
COPY . /build
WORKDIR /build

RUN CGO_ENABLED=0 GOOS=linux go build -v

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /build ./
COPY --from=builder /build/groover-operator ./

ENTRYPOINT ["./groover-operator"]