# Image name — override on the command line: just image=myrepo/myimage build
image := "cybercinch/gravwell-file-follow:latest"

# Show available recipes
default:
    @echo "Welcome,  please select a task:"
    @echo ""
    @just --list

# Build and push the Docker image
build:
    @echo "Building Docker image: {{image}}"
    @docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t {{image}} \
    --push .

# Build and push with a custom CA certificate
# Usage: just build-ca path/to/ca.crt
build-ca ca:
    @echo "Building Docker image with custom CA: {{ca}}"
    @docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --build-arg CUSTOM_CA={{ca}} \
    -t {{image}} \
    --push .
