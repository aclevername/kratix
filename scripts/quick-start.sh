#!/usr/bin/env bash

set -euo pipefail

ROOT=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )/.." &> /dev/null && pwd )
source "${ROOT}/scripts/utils.sh"
source "${ROOT}/scripts/install-gitops"

BUILD_KRATIX_IMAGES=false
RECREATE=false
SINGLE_DESTINATION=false
THIRD_DESTINATION=false

INSTALL_AND_CREATE_MINIO_BUCKET=true
INSTALL_AND_CREATE_GITEA_REPO=false
WORKER_STATESTORE_TYPE=BucketStateStore

LOCAL_IMAGES_DIR=""
VERSION=${VERSION:-"$(cd $ROOT; git branch --show-current)"}
DOCKER_BUILDKIT="${DOCKER_BUILDKIT:-1}"

WAIT_TIMEOUT="180s"
KIND_IMAGE="${KIND_IMAGE:-"kindest/node:v1.27.3"}"

LABELS=true

usage() {
    echo -e "Usage: quick-start.sh [--help] [--recreate] [--local] [--git] [--git-and-minio] [--local-images <location>]"
    echo -e "\t--help, -h            Prints this message"
    echo -e "\t--recreate, -r        Deletes pre-existing KinD clusters"
    echo -e "\t--local, -l           Build and load Kratix images to KinD cache"
    echo -e "\t--local-images, -i    Load container images from a local directory into the KinD clusters"
    echo -e "\t--git, -g             Use Gitea as local repository in place of default local MinIO"
    echo -e "\t--single-cluster, -s  Deploy Kratix on a Single cluster setup"
    echo -e "\t--third-cluster, -t   Deploy Kratix with a three cluster setup"
    echo -e "\t--git-and-minio, -d   Install Gitea alongside the minio installation. Destinations still uses minio as statestore. Can't be used alongside --git"
    echo -e "\t--no-labels, -n       Don't apply any labels to the KinD clusters"
    exit "${1:-0}"
}

load_options() {
    for arg in "$@"; do
      shift
      case "$arg" in
        '--help')           set -- "$@" '-h'   ;;
        '--recreate')       set -- "$@" '-r'   ;;
        '--local')          set -- "$@" '-l'   ;;
        '--git')            set -- "$@" '-g'   ;;
        '--git-and-minio')  set -- "$@" '-d'   ;;
        '--local-images')   set -- "$@" '-i'   ;;
        '--no-labels')      set -- "$@" '-n'   ;;
        '--single-cluster') set -- "$@" '-s'   ;;
        '--third-cluster')  set -- "$@" '-t'   ;;
        *)                  set -- "$@" "$arg" ;;
      esac
    done

    OPTIND=1
    while getopts "hrlgtdi:sn" opt
    do
      case "$opt" in
        'r') RECREATE=true ;;
        's') SINGLE_DESTINATION=true ;;
        't') THIRD_DESTINATION=true ;;
        'h') usage ;;
        'l') BUILD_KRATIX_IMAGES=true ;;
        'n') LABELS=false ;;
        'i') LOCAL_IMAGES_DIR=${OPTARG} ;;
        'd') INSTALL_AND_CREATE_GITEA_REPO=true INSTALL_AND_CREATE_MINIO_BUCKET=true WORKER_STATESTORE_TYPE=BucketStateStore ;;
        'g') INSTALL_AND_CREATE_GITEA_REPO=true INSTALL_AND_CREATE_MINIO_BUCKET=false WORKER_STATESTORE_TYPE=GitStateStore ;;
        *) usage 1 ;;
      esac
    done
    shift $(expr $OPTIND - 1)

    # Always build local images and regenerate distribution when running from `dev`
    if [ "${VERSION}" != "main" ]; then
        VERSION="dev"
    fi
    if [ "${VERSION}" = "dev" ]; then
        BUILD_KRATIX_IMAGES=true
        log -n "Generating local Kratix distribution..."
        run make distribution
    fi
}

verify_prerequisites() {
    exit_code=0
    log -n "Looking for KinD..."
    if ! which kind > /dev/null; then
        error "KinD not found in PATH"
        exit_code=1
    else
        success_mark
    fi

    log -n "Looking for kubectl..."
    if ! which kubectl > /dev/null; then
        error "kubectl not found in PATH"
        exit_code=1
    else
        success_mark
    fi

    log -n "Looking for docker..."
    if ! which docker > /dev/null; then
        error "docker not found in PATH"
        exit_code=1
    else
        success_mark
    fi

    if [[ exit_code -gt 0 ]]; then
        log "\nPlease confirm you have the above prerequisites before re-running this script"
        exit $((exit_code))
    fi

    log -n "Looking for distribution/kratix.yaml... "
    if [ ! -f "${ROOT}/distribution/kratix.yaml" ]; then
        error " not found"
        log "\tEnsure you are on the $(info main) branch or run $(info make distribution)"
        exit 1
    fi
    success_mark

    if ${RECREATE}; then
        log -n "Deleting pre-existing clusters..."
        run kind delete clusters platform worker
        if ${THIRD_DESTINATION}; then
            log -n "Deleting third destination..."
            run kind delete clusters worker-2
        fi
    else
        log -n "Verifying no clusters exist..."
        if kind get clusters 2>&1 | grep --quiet --regexp "platform\|worker"; then
            error_mark
            log ""
            log "🚨 Please ensure there's no KinD clusters named $(info platform) or $(info worker)."
            log "You can run this script with $(info --recreate)"
            log "Or you can manually remove the current clusters by running: "
            log "\tkind delete clusters platform worker"
            exit 1
        fi
    fi
}

_build_kratix_image() {
    docker_org=syntasso
    if ${KRATIX_DEVELOPER:-false}; then
        docker_org=syntassodev
    fi
    docker build --tag $docker_org/kratix-platform:${VERSION} --quiet --file ${ROOT}/Dockerfile ${ROOT} &&
    kind load docker-image $docker_org/kratix-platform:${VERSION} --name platform
}

_build_work_creator_image() {
    docker_org=syntasso
    if ${KRATIX_DEVELOPER:-false}; then
        docker_org=syntassodev
    fi
    docker build --tag $docker_org/kratix-platform-pipeline-adapter:${VERSION} --quiet --file ${ROOT}/Dockerfile.pipeline-adapter ${ROOT} &&
    kind load docker-image $docker_org/kratix-platform-pipeline-adapter:${VERSION} --name platform
}

build_and_load_local_images() {
    export DOCKER_BUILDKIT

    log -n "Building and loading Kratix image locally..."
    if ! run _build_kratix_image; then
        error "Failed to build Kratix image"
        exit 1;
    fi

    log -n "Building and loading Work Creator image locally..."
    if ! run _build_work_creator_image; then
        error "Failed to build Work Creator image"
        exit 1;
    fi
}

patch_image() {
    if ${KRATIX_DEVELOPER:-false}; then
        sed "s_syntasso/kratix_syntassodev/kratix_g"
    else
        cat
    fi
}

patch_statestore() {
    sed "s_BucketStateStore_${WORKER_STATESTORE_TYPE}_g"
}

setup_platform_destination() {
    if ${INSTALL_AND_CREATE_GITEA_REPO}; then
        kubectl --context kind-platform apply --filename "${ROOT}/hack/platform/gitea-install.yaml"
    fi

    if ${INSTALL_AND_CREATE_MINIO_BUCKET}; then
        kubectl --context kind-platform apply --filename "${ROOT}/hack/platform/minio-install.yaml"
    fi

    cat "${ROOT}/distribution/kratix.yaml" | patch_image | kubectl --context kind-platform apply --filename -
}

setup_worker_destination() {
    if ${INSTALL_AND_CREATE_GITEA_REPO}; then
       kubectl --context kind-platform apply --filename "${ROOT}/config/samples/platform_v1alpha1_gitstatestore.yaml"
    fi

    if ${INSTALL_AND_CREATE_MINIO_BUCKET}; then
       kubectl --context kind-platform apply --filename "${ROOT}/config/samples/platform_v1alpha1_bucketstatestore.yaml"
    fi

    if ${SINGLE_DESTINATION}; then
        ${ROOT}/scripts/register-destination --name platform-cluster --context kind-platform
    else
        cat "${ROOT}/config/samples/platform_v1alpha1_worker.yaml" | patch_statestore | kubectl --context kind-platform apply --filename -
        install_gitops kind-worker worker-1
        if ! ${LABELS}; then
            kubectl --context kind-platform label destination worker-1 environment-
        fi
    fi
}

setup_worker_2_destination() {
    install_gitops kind-worker-2 worker-2
}

wait_for_gitea() {
    kubectl wait pod --context kind-platform -n gitea --selector app=gitea --for=condition=ready ${opts}
}

wait_for_minio() {
    while ! kubectl get pods --context kind-platform -n kratix-platform-system | grep minio; do
        sleep 1
    done
    kubectl wait pod --context kind-platform -n kratix-platform-system --selector run=minio --for=condition=ready ${opts}
}

wait_for_local_repository() {
    local timeout_flag="${1:-""}"
    opts=""
    if [ -z "${timeout_flag}" ]; then
        opts="--timeout=${WAIT_TIMEOUT}"
    fi

    if ${INSTALL_AND_CREATE_GITEA_REPO}; then
        wait_for_gitea
    fi

    if ${INSTALL_AND_CREATE_MINIO_BUCKET}; then
        wait_for_minio
    fi
}

wait_for_namespace() {
    local timeout_flag="${1:-""}"
    loops=0
    local context="kind-worker"
    if ${SINGLE_DESTINATION}; then
        context="kind-platform"
    fi
    while ! kubectl --context $context get namespace kratix-worker-system >/dev/null 2>&1; do
        if [ -z "${timeout_flag}" ] && (( loops > 20 )); then
            return 1
        fi
        sleep 5
        loops=$(( loops + 1 ))
    done
    return 0
}

load_kind_image() {
    pushd "${LOCAL_IMAGES_DIR}" > /dev/null
    kind_image=$(ls kindest*.tar)
    load_output=$(docker load --input ${kind_image} | tail -1)

    if [[ "${load_output}" =~ "Loaded image ID" ]]; then
        image_id=$(echo "${load_output}" | cut -d":" -f 3 | tr -d " ")
        image_tag="$(echo "${kind_image%.tar}" | sed "s/__/\//g")"
        docker tag "$image_id" "$image_tag"
    fi
    popd > /dev/null
}

load_images() {
    ps_ids=()
    local destination="$1"
    pushd ${LOCAL_IMAGES_DIR} > /dev/null
    for image_tar in $(ls *.tar | grep -v kindest); do
        kind load image-archive --name "$destination" "$image_tar" &
        ps_ids+=("$!")
    done
    popd > /dev/null

    for ps_id in "${ps_ids[@]}"; do
        wait $ps_id
    done
}

exec_load_images() {
    docker exec -t $1-control-plane bash -c "for file in /images/*; do ctr --namespace=k8s.io image import \$file --all-platforms & done; wait" >/dev/null 2>&1
}

install_kratix() {
    verify_prerequisites

    if ${KRATIX_DEVELOPER:-false}; then
        log -n "Checking image cache is up-to-date\n"
        mkdir -p $ROOT/image-cache/
        $ROOT/scripts/download-image-cache.bash
    fi

    if [ -d "${LOCAL_IMAGES_DIR}" ]; then
        log -n "Loading KinD images... "
        if ! run load_kind_image; then
            error "Failed to load KinD image"
            exit 1;
        fi
    fi
    log -n "Creating platform destination..."
    if ! run kind create cluster --name platform --image $KIND_IMAGE \
        --config <(sed "s.IMAGE_CACHE_DIR.$ROOT/image-cache/.g" ${ROOT}/config/samples/kind-platform-config.yaml)
    then
        error "Could not create platform destination"
        exit 1
    fi

    exec_load_images platform & # we can let this run in background while we build kratix images

    if ${BUILD_KRATIX_IMAGES}; then
        build_and_load_local_images
    fi

    wait

    if [ -d "${LOCAL_IMAGES_DIR}" ]; then
        log -n "Loading images in platform destination..."
        if ! run load_images platform; then
            error "Failed to load images in platform destination"
            exit 1;
        fi
    fi

    log -n "Setting up platform destination..."
    if ! run setup_platform_destination; then
        error " failed"
        exit 1
    fi

    if ! $SINGLE_DESTINATION; then
        log -n "Creating worker destination..."
        if ! run kind create cluster --name worker --image $KIND_IMAGE \
            --config <(sed "s.IMAGE_CACHE_DIR.$ROOT/image-cache/.g" ${ROOT}/hack/destination/kind-worker-config.yaml)
        then
            error "Could not create worker destination"
            exit 1
        fi
        exec_load_images worker &
    fi


    if $THIRD_DESTINATION; then
        log -n "Creating worker destination..."
        if ! run kind create cluster --name worker-2 --image $KIND_IMAGE \
            --config <(sed "s.IMAGE_CACHE_DIR.$ROOT/image-cache/.g" ${ROOT}/config/samples/kind-worker-2-config.yaml)
        then
            error "Could not create worker destination 2"
            exit 1
        fi
        exec_load_images worker-2 &
    fi

    log -n "Waiting for local repository to be running..."
    if ! SUPRESS_OUTPUT=true run wait_for_local_repository; then
        log "\n\nIt's taking longer than usual for the local repository to start."
        log "You can check the platform pods to ensure there are no errors."
        log "This script will continue to wait for the local repository to come up. You can kill it with $(info "CTRL+C.")"
        log -n "\nWaiting for the local repository to be running... "
        run wait_for_local_repository --no-timeout
    fi

    if ! $SINGLE_DESTINATION; then
        if [ -d "${LOCAL_IMAGES_DIR}" ]; then
            log -n "Loading images in worker destination..."
            if ! run load_images worker; then
                error "Failed to load images in worker destination"
                exit 1;
            fi
        fi
    fi

    if $THIRD_DESTINATION; then
        if [ -d "${LOCAL_IMAGES_DIR}" ]; then
            log -n "Loading images in worker destination..."
            if ! run load_images worker-2; then
                error "Failed to load images in worker destination"
                exit 1;
            fi
        fi
    fi

    wait

    log -n "Setting up worker destination..."
    if ! run setup_worker_destination; then
        error " failed"
        exit 1
    fi

    if $THIRD_DESTINATION; then
        if ! run setup_worker_2_destination; then
            error " failed"
            exit 1
        fi
    fi

    log -n "Waiting for system to reconcile... "
    if ! SUPRESS_OUTPUT=true run wait_for_namespace; then
        log "\n\nIt's taking longer than usual for the system to reconcile."
        log "You can check the pods on the platform and worker Destinations for debugging information."
        log "This script will continue to wait. You can kill it with $(info "CTRL+C.")"
        log -n "\nWaiting for local repository to be running... "
        run wait_for_namespace --no-timeout
    fi

    kubectl config use-context kind-platform >/dev/null

    if ${INSTALL_AND_CREATE_MINIO_BUCKET}; then
        kubectl delete job minio-create-bucket -n default --context kind-platform >/dev/null
    fi

    if ${INSTALL_AND_CREATE_GITEA_REPO}; then
        kubectl delete job gitea-create-repository -n gitea --context kind-platform >/dev/null
    fi

    success "Kratix installation is complete!"

    if ! ${KRATIX_DEVELOPER:-false}; then
        echo ""
        echo "If you are following the docs available at kratix.io, make sure to set the following environment variables:"
        echo "export PLATFORM=kind-platform"
        if ${SINGLE_DESTINATION}; then
            echo "export WORKER=kind-platform"
        else
            echo "export WORKER=kind-worker"
        fi
    fi

}

main() {
    load_options $@
    install_kratix
}


if [ "$0" = "${BASH_SOURCE[0]}" ]; then
    main $@
fi
