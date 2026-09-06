#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: release-container.sh check <version-image> <recorded-digest> <github-output>" >&2
  echo "       release-container.sh promote <version-image> <staging-image> <staging-digest> <github-output>" >&2
  echo "       release-container.sh record <tag> <asset-name> <digest>" >&2
  exit 2
}

inspect_digest() {
  local image=$1 output=$2 error=$3 digest
  if docker buildx imagetools inspect "$image" >"$output" 2>"$error"; then
    digest=$(awk '$1 == "Digest:" { print $2; exit }' "$output")
    [[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]] || {
      echo "Could not read the registry digest for $image" >&2
      cat "$output" >&2
      exit 1
    }
    printf '%s\n' "$digest"
    return 0
  fi
  if grep -Eiq 'manifest unknown|name unknown|not found|no such manifest' "$error"; then
    return 2
  fi
  echo "Unable to determine whether $image already exists" >&2
  cat "$error" >&2
  exit 1
}

check_image() {
  local image=${1:-} recorded=${2:-} output=${3:-}
  [[ -n "$image" && -n "$output" ]] || usage
  [[ -z "$recorded" || "$recorded" =~ ^sha256:[a-f0-9]{64}$ ]] || usage

  local version=${image##*:} repository=${image%:*} staging
  local temporary inspect_file error_file existing="" staging_digest="" needs_record=false inspect_status
  staging="$repository:${version}-staging"
  temporary=$(mktemp -d)
  inspect_file="$temporary/inspect"
  error_file="$temporary/error"
  trap "rm -rf -- $(printf '%q' "$temporary")" EXIT

  set +e
  existing=$(inspect_digest "$image" "$inspect_file" "$error_file")
  inspect_status=$?
  set -e
  if [[ "$inspect_status" == 0 ]]; then
    if [[ -n "$recorded" ]]; then
      [[ "$existing" == "$recorded" ]] || {
        echo "Immutable image $image is $existing, but the release records $recorded" >&2
        exit 1
      }
    else
      set +e
      staging_digest=$(inspect_digest "$staging" "$inspect_file" "$error_file")
      inspect_status=$?
      set -e
      if [[ "$inspect_status" == 0 ]]; then
        [[ "$existing" == "$staging_digest" ]] || {
          echo "Image $image already exists without a matching recorded or staging digest" >&2
          exit 1
        }
        needs_record=true
      elif [[ "$inspect_status" == 2 ]]; then
        echo "Image $image already exists without verifiable release digest metadata" >&2
        exit 1
      else
        exit "$inspect_status"
      fi
    fi
    {
      echo "push=false"
      echo "existing_digest=$existing"
      echo "staging_image=$staging"
      echo "needs_record=$needs_record"
    } >>"$output"
    return
  elif [[ "$inspect_status" != 2 ]]; then
    exit "$inspect_status"
  fi

  [[ -z "$recorded" ]] || {
    echo "Release records $recorded, but immutable image $image is missing" >&2
    exit 1
  }
  {
    echo "push=true"
    echo "existing_digest="
    echo "staging_image=$staging"
    echo "needs_record=true"
  } >>"$output"
}

promote_image() {
  local image=${1:-} staging=${2:-} wanted=${3:-} output=${4:-}
  [[ -n "$image" && -n "$staging" && -n "$output" && "$wanted" =~ ^sha256:[a-f0-9]{64}$ ]] || usage

  local temporary inspect_file error_file existing="" final inspect_status
  temporary=$(mktemp -d)
  inspect_file="$temporary/inspect"
  error_file="$temporary/error"
  trap "rm -rf -- $(printf '%q' "$temporary")" EXIT
  set +e
  existing=$(inspect_digest "$image" "$inspect_file" "$error_file")
  inspect_status=$?
  set -e
  if [[ "$inspect_status" == 0 ]]; then
    [[ "$existing" == "$wanted" ]] || {
      echo "Refusing to overwrite immutable image $image ($existing) with $wanted" >&2
      exit 1
    }
  elif [[ "$inspect_status" == 2 ]]; then
    docker buildx imagetools create --tag "$image" "$staging@$wanted"
  else
    exit "$inspect_status"
  fi
  final=$(inspect_digest "$image" "$inspect_file" "$error_file") || {
    echo "Published image $image could not be read back" >&2
    exit 1
  }
  [[ "$final" == "$wanted" ]] || {
    echo "Published image $image has $final, expected $wanted" >&2
    exit 1
  }
  echo "digest=$final" >>"$output"
}

record_digest() {
  local tag=${1:-} asset=${2:-} digest=${3:-}
  [[ -n "$tag" && -n "$asset" && "$digest" =~ ^sha256:[a-f0-9]{64}$ ]] || usage
  local path
  path="${RUNNER_TEMP:-/tmp}/$asset"
  printf '%s\n' "$digest" >"$path"
  gh release upload "$tag" "$path" --clobber
}

case "${1:-}" in
  check)
    shift
    check_image "$@"
    ;;
  promote)
    shift
    promote_image "$@"
    ;;
  record)
    shift
    record_digest "$@"
    ;;
  *) usage ;;
esac
