FROM golang:1.26.6-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" -o /ubiquum ./cmd/gateway
RUN echo "hosts: files dns" > /etc/nsswitch.conf
RUN mkdir -p /home/ubiquum/.ubiquum && chown -R 65532:65532 /home/ubiquum

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/nsswitch.conf /etc/nsswitch.conf
COPY --from=builder /ubiquum /ubiquum
COPY --from=builder --chown=65532:65532 /home/ubiquum /home/ubiquum
ENV HOME=/home/ubiquum
ENV UBIQUUM_HOME=/home/ubiquum/.ubiquum
WORKDIR /home/ubiquum
USER 65532:65532
EXPOSE 4000
ENTRYPOINT ["/ubiquum"]
CMD ["serve", "-config", "/etc/ubiquum/gateway.yaml"]
