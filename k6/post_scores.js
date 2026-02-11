import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const SEASON_ID = __ENV.SEASON_ID || "s1";

export const options = {
  vus: Number(__ENV.VUS || 100),
  duration: __ENV.DURATION || "30s",
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<200"],
  },
};

export default function () {
  const userId = `u${__VU}-${__ITER}`;
  const url = `${BASE_URL}/v1/seasons/${SEASON_ID}/scores`;
  const payload = JSON.stringify({ userId, delta: 1 });
  const params = { headers: { "Content-Type": "application/json" } };

  const res = http.post(url, payload, params);

  if (res.status !== 202) {
    console.log(`Failed status: ${res.status}, Body: ${res.body}`);
  }

  check(res, { "status is 202": (r) => r.status === 202 });
}
