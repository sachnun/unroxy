use std::time::Duration;

pub const DEFAULT_PORT: u16 = 8080;
pub const REMOTE_SERVER_LIST_DECOMPRESSED_LIMIT: u64 = 64 << 20;
pub const EXIT_CACHE_ENTRIES: usize = 4096;
pub const TUNNEL_REFRESH_INTERVAL: Duration = Duration::from_secs(600);
pub const STATE_REFRESH_INTERVAL: Duration = Duration::from_secs(2);
pub const TUNNEL_REFRESH_COUNT: usize = 1;
pub const REMOTE_SERVER_LIST_TIMEOUT: Duration = Duration::from_secs(60);
pub const REMOTE_SERVER_LIST_LIMIT: u64 = 16 << 20;
pub const SERVER_ENTRY_CACHE: &str = "/tmp/unroxy-psiphon/server_entries.txt";
pub const DATA_DIR: &str = "/tmp/unroxy-psiphon";

pub const REMOTE_SERVER_LIST_URLS: [&str; 1] =
    ["https://s3.amazonaws.com/psiphon/web/mjr4-p23r-puwl/server_list_compressed"];

pub const REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY: &str = "MIICIDANBgkqhkiG9w0BAQEFAAOCAg0AMIICCAKCAgEAt7Ls+/39r+T6zNW7GiVpJfzq/xvL9SBH5rIFnk0RXYEYavax3WS6HOD35eTAqn8AniOwiH+DOkvgSKF2caqk/y1dfq47Pdymtwzp9ikpB1C5OfAysXzBiwVJlCdajBKvBZDerV1cMvRzCKvKwRmvDmHgphQQ7WfXIGbRbmmk6opMBh3roE42KcotLFtqp0RRwLtcBRNtCdsrVsjiI1Lqz/lH+T61sGjSjQ3CHMuZYSQJZo/KrvzgQXpkaCTdbObxHqb6/+i1qaVOfEsvjoiyzTxJADvSytVtcTjijhPEV6XskJVHE1Zgl+7rATr/pDQkw6DPCNBS1+Y6fy7GstZALQXwEDN/qhQI9kWkHijT8ns+i1vGg00Mk/6J75arLhqcodWsdeG/M/moWgqQAnlZAGVtJI1OgeF5fsPpXu4kctOfuZlGjVZXQNW34aOzm8r8S0eVZitPlbhcPiR4gT/aSMz/wd8lZlzZYsje/Jr8u/YtlwjjreZrGRmG8KMOzukV3lLmMppXFMvl4bxv6YFEmIuTsOhbLTwFgh7KYNjodLj/LsqRVfwz31PgWQFTEPICV7GCvgVlPRxnofqKSjgTWI4mxDhBpVcATvaoBl1L/6WLbFvBsoAUBItWwctO2xalKxF5szhGm8lccoc5MZr8kfE0uxMgsxz4er68iCID+rsCAQM=";

pub fn data_dir(region: &str) -> String {
    format!("{DATA_DIR}-{region}")
}
