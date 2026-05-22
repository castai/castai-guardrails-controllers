# Build & Deploy Instructions

## ✅ Build Successful!

The Docker images built successfully on your machine.

## 🔧 Option 1: Use Docker Hub (Recommended)

### Step 1: Login to Docker Hub
```bash
docker login
# Enter your Docker Hub username and password/token
```

### Step 2: Tag with YOUR username
```bash
# For TSC Controller
docker tag castai/tsc-controller:latest YOUR_DOCKER_USERNAME/tsc-controller:latest
docker push YOUR_DOCKER_USERNAME/tsc-controller:latest

# For JVM Probe Controller
docker tag castai/jvm-probe-controller:latest YOUR_DOCKER_USERNAME/jvm-probe-controller:latest
docker push YOUR_DOCKER_USERNAME/jvm-probe-controller:latest

# For PDB Controller
docker tag castai/pdb-controller:latest YOUR_DOCKER_USERNAME/pdb-controller:latest
docker push YOUR_DOCKER_USERNAME/pdb-controller:latest
```

### Step 3: Update deployment
Edit `manifests/30-deployment.yaml`:
- Replace `castai/tsc-controller:latest` with `YOUR_DOCKER_USERNAME/tsc-controller:latest`
- Replace `castai/jvm-probe-controller:latest` with `YOUR_DOCKER_USERNAME/jvm-probe-controller:latest`
- Replace `castai/pdb-controller:latest` with `YOUR_DOCKER_USERNAME/pdb-controller:latest`

### Step 4: Deploy to cluster
```bash
kubectl apply -f manifests/30-deployment.yaml
```

---

## 🔧 Option 2: Use GitHub Container Registry (Free)

```bash
# Login to GitHub Packages
echo $GITHUB_TOKEN | docker login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin

# Tag and push
docker tag castai/tsc-controller:latest ghcr.io/YOUR_GITHUB_USERNAME/tsc-controller:latest
docker push ghcr.io/YOUR_GITHUB_USERNAME/tsc-controller:latest

docker tag castai/pdb-controller:latest ghcr.io/YOUR_GITHUB_USERNAME/pdb-controller:latest
docker push ghcr.io/YOUR_GITHUB_USERNAME/pdb-controller:latest
```

---

## 🔧 Option 3: Use GitHub Actions (Automatic)

Already configured in `.github/workflows/build-and-push.yaml`.

Add secrets to GitHub:
1. Go to: https://github.com/castai/castai-guardrails-controllers/settings/secrets/actions
2. Add `DOCKER_USERNAME` = your Docker Hub username
3. Add `DOCKER_TOKEN` = your Docker Hub access token

Push to main to trigger build automatically.

---

## 🔧 Option 4: Load into Kind/Minikube (Local Testing)

```bash
# For Kind cluster
kind load docker-image castai/tsc-controller:latest
docker tag castai/jvm-probe-controller:latest castai/jvm-probe-controller:latest
kind load docker-image castai/jvm-probe-controller:latest
docker tag castai/pdb-controller:latest castai/pdb-controller:latest
kind load docker-image castai/pdb-controller:latest

# Then update deployment to use local image
kubectl apply -f manifests/30-deployment.yaml
```

---

## ✅ After Deploying

```bash
# Watch the logs
kubectl logs -n castai-agent -l app.kubernetes.io/component=tsc-controller -f
kubectl logs -n castai-agent -l app.kubernetes.io/component=jvm-probe-controller -f
kubectl logs -n castai-agent -l app.kubernetes.io/component=pdb-controller -f

# Verify TSC is added
./test-apps/watch-pod-spread.sh
```
