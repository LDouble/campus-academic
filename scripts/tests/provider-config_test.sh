#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
config=$repo_root/deploy/provider-ouc.json

jq -e '
  .version == 1 and
  .portal_service_url == "https://my.ouc.edu.cn/manage/common/cas_login/2?redirect=https%3A%2F%2Fmy.ouc.edu.cn%2Ffrontend%2Fuser%2Finfo" and
  .undergraduate.course_catalog.path == "/jsxsd/xkgl/loadXkkbList" and
  .graduate.courses.path == "/py/page/student/grkcb.htm?zc=-1" and
  .graduate.courses_fallback.path == "/py/page/student/xkgrcx.htm" and
  .graduate.course_catalog.path == "/py/page/student/lnsjCxdc.htm"
' "$config" >/dev/null

echo 'provider config tests passed'
