FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ztpd .

FROM alpine:3.20
# not scratch: you'll want a shell in here when a switch won't provision
RUN adduser -D -H ztpd && mkdir /data && chown ztpd /data
COPY --from=build /ztpd /usr/local/bin/ztpd
COPY examples /etc/ztpd
USER ztpd
WORKDIR /etc/ztpd
ENV ZTPD_LOG=/data/events.jsonl
EXPOSE 8080
ENTRYPOINT ["ztpd"]
CMD ["serve"]
