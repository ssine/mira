#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: release-state.sh prepare <draft|publish|promote> <commit> <github-output>" >&2
  echo "       release-state.sh sync-assets <tag> <draft|published> <directory>" >&2
  exit 2
}

repo=${GH_REPO:-${GITHUB_REPOSITORY:-}}
api_url=${GITHUB_API_URL:-https://api.github.com}
[[ -n "$repo" && -n "${GH_TOKEN:-}" ]] || {
  echo "GH_REPO/GITHUB_REPOSITORY and GH_TOKEN are required" >&2
  exit 2
}

github_json() {
  local endpoint=$1 description=$2 output=$3 status
  status=$(curl --silent --show-error --retry 3 \
    --output "$output" --write-out '%{http_code}' \
    --header "Authorization: Bearer $GH_TOKEN" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    "$api_url/repos/$repo/$endpoint")
  case "$status" in
    200|404) printf '%s\n' "$status" ;;
    *)
      echo "GitHub lookup for $description returned HTTP $status" >&2
      cat "$output" >&2
      exit 1
      ;;
  esac
}

release_json() {
  local tag=$1 output=$2 status releases matches count
  status=$(github_json "releases/tags/$tag" "release $tag" "$output")
  if [[ "$status" == 200 ]]; then
    echo 200
    return
  fi

  # GitHub's release-by-tag endpoint omits drafts whose git tag has not been
  # created yet. Acceptance drafts are intentionally created before the tag,
  # so fall back to the authenticated release listing and require one exact
  # match. This also makes a failed acceptance run safe to retry.
  releases="${output}.releases"
  matches="${output}.matches"
  # A newly created draft can take a few seconds to appear in the list. The
  # bounded retry also prevents an immediate workflow retry from creating a
  # second draft while GitHub is converging.
  for attempt in 1 2 3 4 5 6; do
    gh api --paginate --slurp "repos/$repo/releases?per_page=100" >"$releases"
    jq --arg tag "$tag" '[.[][] | select(.tag_name == $tag)]' "$releases" >"$matches"
    count=$(jq 'length' "$matches")
    case "$count" in
      1)
        jq '.[0]' "$matches" >"$output"
        echo 200
        return
        ;;
      0) ;;
      *)
        echo "Multiple GitHub releases use tag $tag" >&2
        exit 1
        ;;
    esac
    [[ "$attempt" == 6 ]] || sleep 2
  done
  echo 404
}

decimal_less() {
  local left=$1 right=$2
  if (( ${#left} < ${#right} )); then return 0; fi
  if (( ${#left} > ${#right} )); then return 1; fi
  [[ "$left" < "$right" ]]
}

stable_semver_less() {
  local left=$1 right=$2 left_major left_minor left_patch right_major right_minor right_patch
  IFS=. read -r left_major left_minor left_patch <<<"$left"
  IFS=. read -r right_major right_minor right_patch <<<"$right"
  decimal_less "$left_major" "$right_major" && return 0
  decimal_less "$right_major" "$left_major" && return 1
  decimal_less "$left_minor" "$right_minor" && return 0
  decimal_less "$right_minor" "$left_minor" && return 1
  decimal_less "$left_patch" "$right_patch"
}

remote_tag_commit() {
  local tag=$1 output=$2 ref="refs/tags/$tag" status
  set +e
  git ls-remote --exit-code --refs origin "$ref" >"$output"
  status=$?
  set -e
  case "$status" in
    0)
      git fetch --force --no-tags origin "$ref:$ref" >/dev/null
      git rev-parse "${ref}^{commit}"
      ;;
    2) return 2 ;;
    *)
      echo "Unable to inspect remote tag $tag" >&2
      return 1
      ;;
  esac
}

prepare() {
  local mode=${1:-} requested_commit=${2:-} output=${3:-}
  case "$mode" in draft|publish|promote) ;; *) usage ;; esac
  [[ -n "$requested_commit" && -n "$output" ]] || usage

  local commit version tag temporary release_file latest_file tag_file status state target actual tag_status
  local latest_status latest_tag latest_version
  commit=$(git rev-parse "${requested_commit}^{commit}")
  [[ "$commit" =~ ^[a-f0-9]{40}$ && "$(git rev-parse HEAD)" == "$commit" ]] || {
    echo "Release checkout does not match requested commit $requested_commit" >&2
    exit 1
  }
  version=$(<VERSION)
  [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "VERSION must be stable SemVer major.minor.patch; prereleases cannot move latest" >&2
    exit 1
  }
  tag="v$version"
  temporary=$(mktemp -d)
  release_file="$temporary/release.json"
  latest_file="$temporary/latest.json"
  tag_file="$temporary/tag"
  trap "rm -rf -- $(printf '%q' "$temporary")" EXIT

  status=$(release_json "$tag" "$release_file")
  set +e
  actual=$(remote_tag_commit "$tag" "$tag_file")
  tag_status=$?
  set -e
  case "$tag_status" in
    0)
      [[ "$actual" == "$commit" ]] || {
        echo "Remote tag $tag points to $actual, expected $commit" >&2
        exit 1
      }
      ;;
    2) ;;
    *) exit "$tag_status" ;;
  esac

  if [[ "$mode" != draft ]]; then
    latest_status=$(github_json releases/latest "latest public stable release" "$latest_file")
    if [[ "$latest_status" == 200 ]]; then
      latest_tag=$(jq -r .tag_name "$latest_file")
      [[ "$latest_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
        echo "Latest public release has unsupported tag $latest_tag" >&2
        exit 1
      }
      [[ "$(jq -r .draft "$latest_file")" == false && "$(jq -r .prerelease "$latest_file")" == false ]] || {
        echo "GitHub latest unexpectedly points to a draft or prerelease" >&2
        exit 1
      }
      latest_version=${latest_tag#v}
      if stable_semver_less "$version" "$latest_version"; then
        echo "Refusing to move latest backward from $latest_tag to $tag" >&2
        exit 1
      fi
    fi
  fi

  if [[ "$status" == 404 ]]; then
    [[ "$mode" != promote ]] || {
      echo "Promotion requires the acceptance draft $tag" >&2
      exit 1
    }
    gh release create "$tag" --target "$commit" --draft --latest=false \
      --generate-notes --title "Mira $version"
    status=$(release_json "$tag" "$release_file")
    [[ "$status" == 200 ]] || {
      echo "Created draft $tag could not be read back" >&2
      exit 1
    }
  fi

  [[ "$(jq -r .tag_name "$release_file")" == "$tag" ]] || {
    echo "GitHub release tag identity does not match $tag" >&2
    exit 1
  }
  [[ "$(jq -r .prerelease "$release_file")" == false ]] || {
    echo "Mira stable release $tag must not be marked as a prerelease" >&2
    exit 1
  }
  if [[ "$(jq -r .draft "$release_file")" == true ]]; then
    state=draft
    target=$(jq -r .target_commitish "$release_file")
    [[ "$target" == "$commit" || ( "$tag_status" == 0 && "$actual" == "$commit" ) ]] || {
      echo "Draft $tag targets $target, expected immutable commit $commit" >&2
      exit 1
    }
  else
    state=published
    [[ "$mode" != draft ]] || {
      echo "Release $tag is already public; refusing a new acceptance run for this version" >&2
      exit 1
    }
    [[ "$tag_status" == 0 ]] || {
      echo "Public release $tag has no matching remote git tag" >&2
      exit 1
    }
  fi

  if [[ "$tag_status" == 2 && "$mode" != draft ]]; then
    git update-ref "refs/tags/$tag" "$commit"
    git push origin "refs/tags/$tag:refs/tags/$tag"
    set +e
    actual=$(remote_tag_commit "$tag" "$tag_file")
    tag_status=$?
    set -e
    [[ "$tag_status" == 0 && "$actual" == "$commit" ]] || {
      echo "Remote tag $tag was not visible at the expected commit after push" >&2
      exit 1
    }
  fi

  local digest_asset="mira_${version}_container.digest" asset_count asset_id recorded_digest=""
  asset_count=$(jq --arg name "$digest_asset" '[.assets[] | select(.name == $name)] | length' "$release_file")
  [[ "$asset_count" -le 1 ]] || {
    echo "Release $tag has duplicate $digest_asset assets" >&2
    exit 1
  }
  if [[ "$asset_count" == 1 ]]; then
    asset_id=$(jq -r --arg name "$digest_asset" '.assets[] | select(.name == $name) | .id' "$release_file")
    gh api --header 'Accept: application/octet-stream' \
      "repos/$repo/releases/assets/$asset_id" >"$release_file.digest"
    recorded_digest=$(tr -d '\r\n' <"$release_file.digest")
    [[ "$recorded_digest" =~ ^sha256:[a-f0-9]{64}$ ]] || {
      echo "Release $tag contains an invalid container digest asset" >&2
      exit 1
    }
  fi

  {
    echo "version=$version"
    echo "tag=$tag"
    echo "release_state=$state"
    echo "digest_asset=$digest_asset"
    echo "recorded_digest=$recorded_digest"
  } >>"$output"
}

sync_assets() {
  local tag=${1:-} state=${2:-} directory=${3:-}
  [[ -n "$tag" && -d "$directory" ]] || usage
  case "$state" in draft|published) ;; *) usage ;; esac

  local assets=() asset name download_dir
  while IFS= read -r -d '' asset; do assets+=("$asset"); done \
    < <(find "$directory" -mindepth 1 -maxdepth 1 -type f -print0 | sort -z)
  [[ ${#assets[@]} -gt 0 ]] || {
    echo "No release assets found in $directory" >&2
    exit 1
  }
  if [[ "$state" == draft ]]; then
    gh release upload "$tag" "${assets[@]}" --clobber
    return
  fi

  download_dir=$(mktemp -d)
  trap "rm -rf -- $(printf '%q' "$download_dir")" EXIT
  for asset in "${assets[@]}"; do
    name=$(basename "$asset")
    gh release download "$tag" --dir "$download_dir" --pattern "$name"
    cmp "$asset" "$download_dir/$name" || {
      echo "Published release asset differs from the verified artifact: $name" >&2
      exit 1
    }
  done
}

case "${1:-}" in
  prepare)
    shift
    prepare "$@"
    ;;
  sync-assets)
    shift
    sync_assets "$@"
    ;;
  *) usage ;;
esac
