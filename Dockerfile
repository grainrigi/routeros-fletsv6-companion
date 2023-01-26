FROM arm32v7/golang:1.19-bullseye

COPY / /src
WORKDIR /src
RUN go build .

FROM gcr.io/distroless/base-debian11
WORKDIR /root/
COPY --from=0 /src/routeros-fletsv6-companion ./

CMD ["./routeros-fletsv6-companion"]
