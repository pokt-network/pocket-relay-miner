"""
Static nginx backend for load testing.
Returns fixed JSON-RPC response with minimal latency.
"""

load("ext://deployment", "deployment_create")

def provision_nginx_backend():
    # High-performance nginx server for static JSON-RPC responses
    deployment_create(
        "nginx-backend",
        image="nginx:alpine",
        command=["sh", "-c"],
        args=[
            """echo '{"jsonrpc":"2.0","id":1,"result":"0x1"}' > /usr/share/nginx/html/index.html && \
echo 'user nginx;
worker_processes auto;
worker_rlimit_nofile 100000;

events {
    worker_connections 65536;
    multi_accept on;
    use epoll;
}

http {
    # Performance settings
    sendfile on;
    tcp_nopush on;
    tcp_nodelay on;

    # Connection optimization
    keepalive_requests 10000;
    keepalive_timeout 300s;

    # Disable logging for maximum performance
    access_log off;
    error_log /var/log/nginx/error.log crit;

    # Buffer optimizations
    client_body_buffer_size 128k;
    client_max_body_size 10m;
    client_header_buffer_size 1k;
    large_client_header_buffers 4 4k;
    output_buffers 1 32k;
    postpone_output 1460;

    # Disable unnecessary features
    gzip off;

    server {
        listen 80 backlog=65535 reuseport;

        # Performance optimizations
        keepalive_requests 10000;
        keepalive_timeout 300s;
        access_log off;
        error_log off;
        tcp_nopush on;
        tcp_nodelay on;

        root /usr/share/nginx/html;
        location / {
            add_header Content-Type application/json always;
            add_header Access-Control-Allow-Origin * always;
            add_header Access-Control-Allow-Methods "GET, POST, OPTIONS" always;
            add_header Access-Control-Allow-Headers "Content-Type" always;
            if ($request_method = OPTIONS) {
                return 200;
            }
            if ($request_method = POST) {
                error_page 405 =200 $uri;
            }
            try_files $uri /index.html;
        }
    }
}' > /etc/nginx/nginx.conf && \
nginx -g 'daemon off;'"""
        ],
        ports="80",
    )

    k8s_resource(
        "nginx-backend",
        labels=["backends"],
        port_forwards=["8548:80"],
        resource_deps=[]
    )
