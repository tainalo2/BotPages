function handle() {
  var response = fetch("https://blocked.example/private");
  return {status: response.status, body: response.body};
}
