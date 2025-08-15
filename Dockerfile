FROM golang:1.24.6-bookworm

WORKDIR /app

ENV GO111MODULE=on
ENV GOPROXY=https://goproxy.cn,direct

COPY . .

RUN go mod download

RUN go build -o main main.go

EXPOSE 8899
CMD ["./main"]


