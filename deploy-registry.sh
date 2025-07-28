#!/bin/bash

# Docker Distribution Registry - Kubernetes Deployment Script
# This script deploys the registry using the configuration template

set -e

# Default configuration file
CONFIG_FILE="kubernetes-config-template.yaml"
DEPLOYMENT_FILE="kubernetes-deployment.yaml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if required tools are installed
check_requirements() {
    log_info "Checking requirements..."
    
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is not installed or not in PATH"
        exit 1
    fi
    
    if ! command -v yq &> /dev/null; then
        log_warn "yq is not installed. You'll need to manually update the secrets."
    fi
    
    log_info "Requirements check passed"
}

# Function to read configuration from template
read_config() {
    local key=$1
    if command -v yq &> /dev/null; then
        yq eval "$key" "$CONFIG_FILE" 2>/dev/null || echo ""
    else
        echo ""
    fi
}

# Function to create or update secrets
create_secrets() {
    log_info "Creating/updating registry secrets..."
    
    # Read configuration values
    AWS_ACCESS_KEY=$(read_config '.aws.accessKey')
    AWS_SECRET_KEY=$(read_config '.aws.secretKey')
    AWS_REGION=$(read_config '.aws.region')
    S3_BUCKET=$(read_config '.s3.bucket')
    S3_ROOT_DIRECTORY=$(read_config '.s3.rootDirectory')
    S3_ENCRYPT=$(read_config '.s3.encrypt')
    S3_STORAGE_CLASS=$(read_config '.s3.storageClass')
    REDIS_PASSWORD=$(read_config '.redis.password')
    HTTP_SECRET=$(read_config '.security.httpSecret')
    AUTH_TOKEN_REALM=$(read_config '.auth.token.realm')
    AUTH_TOKEN_SERVICE=$(read_config '.auth.token.service')
    AUTH_TOKEN_ISSUER=$(read_config '.auth.token.issuer')
    WEBHOOK_URL=$(read_config '.webhook.url')
    WEBHOOK_TOKEN=$(read_config '.webhook.token')
    
    # Create namespace if it doesn't exist
    kubectl create namespace docker-registry --dry-run=client -o yaml | kubectl apply -f -
    
    # Create/update registry secrets
    kubectl create secret generic registry-secrets \
        --namespace=docker-registry \
        --from-literal=REGISTRY_HTTP_SECRET="${HTTP_SECRET:-change-this-secret-in-production}" \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY:-your-aws-access-key}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_KEY:-your-aws-secret-key}" \
        --from-literal=AWS_REGION="${AWS_REGION:-us-west-2}" \
        --from-literal=S3_BUCKET="${S3_BUCKET:-my-registry-bucket}" \
        --from-literal=S3_ROOT_DIRECTORY="${S3_ROOT_DIRECTORY:-/docker/registry/v2}" \
        --from-literal=S3_ENCRYPT="${S3_ENCRYPT:-true}" \
        --from-literal=S3_STORAGE_CLASS="${S3_STORAGE_CLASS:-STANDARD}" \
        --from-literal=REDIS_PASSWORD="${REDIS_PASSWORD:-redis-password}" \
        --from-literal=AUTH_TOKEN_REALM="${AUTH_TOKEN_REALM:-https://auth.example.com/token}" \
        --from-literal=AUTH_TOKEN_SERVICE="${AUTH_TOKEN_SERVICE:-registry.example.com}" \
        --from-literal=AUTH_TOKEN_ISSUER="${AUTH_TOKEN_ISSUER:-auth.example.com}" \
        --from-literal=WEBHOOK_URL="${WEBHOOK_URL:-https://webhook.example.com/registry}" \
        --from-literal=WEBHOOK_TOKEN="${WEBHOOK_TOKEN:-webhook-bearer-token}" \
        --dry-run=client -o yaml | kubectl apply -f -
    
    log_info "Registry secrets created/updated"
}

# Function to create TLS secrets
create_tls_secrets() {
    log_info "Creating TLS secrets..."
    
    TLS_CERT=$(read_config '.tls.certificate')
    TLS_KEY=$(read_config '.tls.privateKey')
    
    if [[ -n "$TLS_CERT" && -n "$TLS_KEY" ]]; then
        kubectl create secret tls registry-tls \
            --namespace=docker-registry \
            --cert=<(echo "$TLS_CERT" | base64 -d) \
            --key=<(echo "$TLS_KEY" | base64 -d) \
            --dry-run=client -o yaml | kubectl apply -f -
    else
        log_warn "TLS certificate and key not provided in config. Please update the registry-tls secret manually."
    fi
}

# Function to create auth certificate secrets
create_auth_secrets() {
    log_info "Creating auth certificate secrets..."
    
    ROOT_CERT_BUNDLE=$(read_config '.auth.token.rootCertBundle')
    
    if [[ -n "$ROOT_CERT_BUNDLE" ]]; then
        kubectl create secret generic registry-auth-certs \
            --namespace=docker-registry \
            --from-literal=root-cert-bundle.pem="$(echo "$ROOT_CERT_BUNDLE" | base64 -d)" \
            --dry-run=client -o yaml | kubectl apply -f -
    else
        log_warn "Root certificate bundle not provided in config. Please update the registry-auth-certs secret manually."
    fi
}

# Function to deploy the registry
deploy_registry() {
    log_info "Deploying Docker Registry..."
    
    if [[ ! -f "$DEPLOYMENT_FILE" ]]; then
        log_error "Deployment file $DEPLOYMENT_FILE not found"
        exit 1
    fi
    
    kubectl apply -f "$DEPLOYMENT_FILE"
    
    log_info "Registry deployment applied"
}

# Function to wait for deployment to be ready
wait_for_deployment() {
    log_info "Waiting for registry deployment to be ready..."
    
    kubectl wait --for=condition=available --timeout=300s deployment/docker-registry -n docker-registry
    kubectl wait --for=condition=available --timeout=300s deployment/redis -n docker-registry
    
    log_info "Registry deployment is ready"
}

# Function to show deployment status
show_status() {
    log_info "Registry deployment status:"
    echo
    kubectl get all -n docker-registry
    echo
    
    log_info "Registry endpoints:"
    INGRESS_IP=$(kubectl get ingress docker-registry -n docker-registry -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "pending")
    DOMAIN=$(read_config '.registry.domain')
    echo "External URL: https://${DOMAIN:-registry.example.com}"
    echo "Health check: curl -k https://${DOMAIN:-registry.example.com}/v2/"
    echo "Metrics: http://<pod-ip>:5001/metrics"
}

# Function to show usage
usage() {
    echo "Usage: $0 [OPTIONS]"
    echo
    echo "Options:"
    echo "  -c, --config FILE     Use custom configuration file (default: $CONFIG_FILE)"
    echo "  -d, --deploy-only     Only deploy, skip secret creation"
    echo "  -s, --secrets-only    Only create secrets, skip deployment"
    echo "  -w, --wait            Wait for deployment to be ready"
    echo "  --status              Show deployment status"
    echo "  -h, --help            Show this help message"
    echo
    echo "Examples:"
    echo "  $0                           # Full deployment with default config"
    echo "  $0 -c my-config.yaml        # Deploy with custom config"
    echo "  $0 --secrets-only            # Only update secrets"
    echo "  $0 --deploy-only --wait      # Deploy and wait for readiness"
}

# Parse command line arguments
DEPLOY_ONLY=false
SECRETS_ONLY=false
WAIT_FOR_READY=false
SHOW_STATUS=false

while [[ $# -gt 0 ]]; do
    case $1 in
        -c|--config)
            CONFIG_FILE="$2"
            shift 2
            ;;
        -d|--deploy-only)
            DEPLOY_ONLY=true
            shift
            ;;
        -s|--secrets-only)
            SECRETS_ONLY=true
            shift
            ;;
        -w|--wait)
            WAIT_FOR_READY=true
            shift
            ;;
        --status)
            SHOW_STATUS=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

# Main execution
main() {
    log_info "Starting Docker Registry deployment..."
    
    if [[ ! -f "$CONFIG_FILE" ]]; then
        log_error "Configuration file $CONFIG_FILE not found"
        exit 1
    fi
    
    check_requirements
    
    if [[ "$SHOW_STATUS" == "true" ]]; then
        show_status
        exit 0
    fi
    
    if [[ "$SECRETS_ONLY" == "true" ]]; then
        create_secrets
        create_tls_secrets
        create_auth_secrets
        log_info "Secrets created/updated successfully"
        exit 0
    fi
    
    if [[ "$DEPLOY_ONLY" == "false" ]]; then
        create_secrets
        create_tls_secrets
        create_auth_secrets
    fi
    
    deploy_registry
    
    if [[ "$WAIT_FOR_READY" == "true" ]]; then
        wait_for_deployment
        show_status
    fi
    
    log_info "Docker Registry deployment completed successfully!"
    log_warn "Don't forget to update the TLS and auth certificates if needed"
    log_warn "Review and update the configuration secrets for production use"
}

# Run main function
main "$@"