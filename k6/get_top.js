import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const SEASON_ID = __ENV.SEASON_ID || "s1";
const LIMIT = __ENV.LIMIT || "100";
const VUS = Number(__ENV.VUS || 300);
const DURATION = __ENV.DURATION || "60s";

export const options = {
  vus: VUS,
  duration: DURATION,
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<80"],
  },
};

export default function () {
  const url = `${BASE_URL}/v1/seasons/${SEASON_ID}/leaderboard/top?limit=${LIMIT}`;
  const res = http.get(url, {
    headers: { Accept: "application/json" },
  });

  check(res, {
    "status is 200": (r) => r.status === 200,
    "content-type is json": (r) =>
      String(r.headers["Content-Type"] || "").includes("application/json"),
    "seasonId matches": (r) => r.json("seasonId") === SEASON_ID,
  });
}
