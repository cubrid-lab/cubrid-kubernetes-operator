/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import java.io.BufferedReader;
import java.io.IOException;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.nio.file.Files;
import java.nio.file.Path;
import java.sql.Connection;
import java.sql.Driver;
import java.sql.DriverManager;
import java.sql.DriverPropertyInfo;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.logging.Logger;

/** Exercises the actual command in child JVMs; the fake JDBC driver never uses a network. */
public final class WorkloadApplicationReconnectTest {
  private static final String CAPABILITIES =
      "{\"schemaVersion\":1,\"capabilities\":[\"application-reconnect-stop-after-error-v1\","
          + "\"application-connection-diagnostics-v1\"],\"physicalSessionContinuity\":\"unverified\","
          + "\"driverInternalRetryControl\":false}";
  private static final String SECRET = "SECRET-driver-url-password-message-object";
  private static final String URL = "jdbc:workload-policy-test:" + SECRET;
  private static final String ERROR = "code 73: " + SECRET;
  private static int checks;

  private WorkloadApplicationReconnectTest() {}

  public static void main(String[] args) throws Exception {
    if (args.length > 0 && args[0].equals("child")) {
      child(args[1], args[2]);
      return;
    }
    List<String> cases =
        args.length == 0
            ? List.of(
                "capabilities",
                "capabilities_with_env",
                "statement",
                "defaults",
                "default_unknown",
                "diagnostics_only",
                "success",
                "driver_open",
                "configure",
                "prepare",
                "bind",
                "statement_close",
                "rollback",
                "commit",
                "read_query",
                "read_result",
                "read_commit",
                "read_mismatch_commit",
                "read_mismatch",
                "close_failed",
                "close_unknown",
                "internal_retry",
                "invalid",
                "diagnostic_open",
                "diagnostic_flush",
                "diagnostic_returned",
                "diagnostic_ack",
                "diagnostic_failed",
                "diagnostic_unknown",
                "bad_sqlstate",
                "abrupt")
            : List.of(args);
    for (String name : cases) {
      verify(name);
      System.out.println("PASS " + name);
    }
    System.out.println("Passed " + checks + " application policy assertions");
  }

  private static void verify(String name) throws Exception {
    if (name.equals("defaults")) {
      Result unset = launch("statement", null, null, "run");
      Result reconnect = launch("statement", "reconnect", null, "run");
      equal(0, unset.exit, "default exit");
      equal(2, unset.count("opens"), "default reconnects after a statement error");
      equal(normalize(unset.out), normalize(reconnect.out), "explicit reconnect preserves default history");
      equal("", unset.err, "default stderr");
      equal("", reconnect.err, "reconnect stderr");
      equal("", unset.diag, "default has no separate diagnostic file");
      equal("", reconnect.diag, "reconnect has no separate diagnostic file");
      equal(
          history("failed", ERROR, 1) + history("acknowledged", null, 2)
              + history("acknowledged", null, 3),
          normalize(unset.out),
          "default exact history fields, IDs, outcomes and order");
      return;
    }
    if (name.equals("default_unknown")) {
      Result r = launch("commit", null, null, "run");
      equal(0, r.exit, "default commit error continues");
      equal(2, r.count("opens"), "default commit error reconnects");
      equal(history("unknown", ERROR, 1) + history("acknowledged", null, 2)
          + history("acknowledged", null, 3), normalize(r.out), "default commit remains unknown without retry");
      equal("", r.err, "default commit stderr");
      return;
    }
    if (name.equals("diagnostics_only")) {
      Result r = launch("statement", "reconnect", "file", "run");
      equal(0, r.exit, "diagnostics alone preserve reconnect behavior");
      equal(2, r.count("opens"), "diagnostics alone do not limit application opens");
      equal(history("failed", ERROR, 1) + history("acknowledged", null, 2)
          + history("acknowledged", null, 3), normalize(r.out), "diagnostics preserve raw default error text");
      check(r.diag.contains("\"applicationConnectionAttempt\":2"), "diagnostics number reopened application connections");
      check(!r.diag.contains(SECRET), "default policy diagnostics omit raw driver details");
      equal("", r.err, "diagnostics remain separate from stderr");
      return;
    }
    if (name.equals("invalid")) {
      for (String policy : List.of("invalid", "stop-after-error")) {
        Result r = launch("success", policy, null, "run");
        check(r.exit != 0 && r.count("opens") == 0 && r.out.isEmpty(), "invalid/missing diagnostic configuration fails before JDBC");
      }
      for (String command : List.of("init", "dump", "marker")) {
        Result r = launch("success", "stop-after-error", "file", command);
        check(r.exit != 0 && r.count("opens") == 0 && r.out.isEmpty(), "nondefault policy rejected for " + command);
      }
      return;
    }
    String diagnostic = name.equals("capabilities") ? null
        : name.startsWith("diagnostic_") ? name : "file";
    Result r = launch(
        name, "stop-after-error", diagnostic,
        name.startsWith("capabilities") ? "capabilities" : "run");
    if (name.startsWith("capabilities")) {
      equal(0, r.exit, "capabilities must dispatch before requiring JDBC_URL");
      equal(CAPABILITIES + "\n", r.out, "exact no-database capability document");
      equal("", r.err, "capability stderr");
      equal(0, r.count("opens"), "capabilities never connect");
      return;
    }
    if (name.equals("diagnostic_open") || name.equals("diagnostic_flush")) {
      check(r.exit != 0, "unwritable diagnostics fail");
      equal(0, r.count("opens"), "diagnostic failure before JDBC must not connect");
      check(
          !r.out.contains("\"event\":\"failed\"")
              && !r.out.contains("\"event\":\"unknown\"")
              && !r.out.contains("\"event\":\"acknowledged\""),
          "diagnostic failure must not fabricate SQL outcome");
      return;
    }
    if (name.equals("diagnostic_returned")) {
      equal(1, r.exit, "returned-connection diagnostic failure stops the run");
      equal(1, r.count("opens"), "only the initial application open occurs");
      equal(1, r.count("closes"), "returned connection is closed before any statement");
      check(!r.out.contains("\"event\":\"failed\"")
          && !r.out.contains("\"event\":\"unknown\"")
          && !r.out.contains("\"event\":\"acknowledged\""),
          "diagnostics cannot invent a SQL outcome before statements");
      return;
    }
    if (name.equals("abrupt")) {
      equal(7, r.exit, "abrupt child exit");
      check(r.diag.contains("application_connection_attempted")
          && r.diag.contains("application_connection_returned"), "complete records precede abrupt end");
      check(!r.diag.contains("application_connection_closed"), "abrupt end cannot claim closure");
      String tail = r.diag.substring(r.diag.lastIndexOf('\n') + 1);
      equal("{\"schemaVersion\":", tail, "truncated tail remains visibly incomplete");
      return;
    }
    boolean success = List.of("success", "internal_retry", "read_mismatch", "diagnostic_ack").contains(name);
    equal(1, r.count("opens"), "at most one application DriverManager call after " + name);
    equal(name.startsWith("diagnostic_") ? 1 : success ? 0 : 3, r.exit, "policy exit for " + name);
    int outcomes = occurrences(r.out, "\"event\":\"attempted\"");
    equal(success ? 3 : name.startsWith("read_") ? 2 : 1, outcomes, "no later operation after " + name);
    String outcome = name.contains("unknown") || name.equals("commit") ? "unknown" : "failed";
    if (name.equals("success") || name.equals("internal_retry")) {
      equal(
          history("acknowledged", null, 1) + history("acknowledged", null, 2)
              + history("acknowledged", null, 3),
          normalize(r.out),
          "policy preserves success history exactly");
    }
    check(!r.err.contains(SECRET), "opt-in stderr never leaks driver values");
    if (!success) {
      equal(1, occurrences(r.out, "\"event\":\"" + outcome + "\""), "exactly one failed or unknown outcome");
      check(r.out.contains("\"event\":\"" + outcome + "\""), "preserve SQL outcome for " + name);
      check(!r.out.contains(SECRET), "opt-in history must not leak raw driver values");
      if (!name.equals("read_mismatch_commit")) {
        check(r.out.contains("SQLState " + (name.equals("bad_sqlstate") ? "unknown" : "08006") + ", code 73"), "safe SQL summary retains valid state and vendor code");
      } else {
        check(r.out.contains("read mismatch:"), "read mismatch survives a read commit exception");
      }
    }
    if (name.equals("read_mismatch")) {
      check(r.out.contains("read mismatch:"), "derived read mismatch survives");
      check(!r.diag.contains("application_connection_failed"), "data mismatch is not a connection failure");
    }
    if (name.startsWith("read_") && !name.equals("read_mismatch")) {
      check(r.out.contains("\"target\":\"c1-000001\""), "read keeps acknowledged insert target");
    }
    if (name.equals("diagnostic_ack")) {
      equal(3, occurrences(r.out, "\"event\":\"acknowledged\""), "diagnostic close failure retains acknowledged results");
    }
    if (name.equals("internal_retry")) {
      equal(2, r.count("transportAttempts"), "fake driver can replace transport inside one Java Connection");
    }
    if (!name.startsWith("diagnostic_")) {
      validateDiagnostics(r.diag);
      List<String> expectedEvents;
      if (name.equals("driver_open")) {
        expectedEvents = List.of("attempted", "failed", "stopped");
      } else if (success) {
        expectedEvents = List.of("attempted", "returned", "close_attempted", "closed");
      } else {
        expectedEvents = List.of("attempted", "returned", "failed", "close_attempted",
            name.startsWith("close_") ? "close_failed" : "closed", "stopped");
      }
      equal(expectedEvents, r.diag.lines().map(line -> line.replaceAll(
          ".*\"event\":\"application_(connection_|reconnect_policy_)([a-z_]+)\".*", "$2")).toList(),
          "exact application lifecycle events and ordering");
      check(r.diag.contains("\"event\":\"application_connection_attempted\""), "application attempt recorded");
      if (!name.equals("driver_open")) {
        check(r.diag.contains("application_connection_returned"), "returned Java Connection recorded");
        equal(1, r.count("closes"), "returned connection closed best effort, including configure error");
      } else {
        check(!r.diag.contains("application_connection_returned") && !r.diag.contains("application_connection_closed"), "failed open cannot claim returned or closed");
      }
      if (!success) {
        check(r.diag.contains("application_reconnect_policy_stopped"), "stop event recorded");
        String stage = switch (name) {
          case "prepare", "bind", "statement_close", "close_failed", "bad_sqlstate" -> "statement";
          case "close_unknown" -> "commit";
          case "read_query", "read_result" -> "read";
          case "read_commit", "read_mismatch_commit" -> "commit";
          default -> name;
        };
        check(r.diag.contains("\"event\":\"application_connection_failed\",\"stage\":\"" + stage + "\""), "SQL failure stage " + stage);
      }
      if (name.startsWith("close_")) {
        check(r.diag.contains("application_connection_close_failed") && !r.diag.contains("application_connection_closed"), "close failure never masks original outcome or claims closure");
      }
    }
  }

  private static void validateDiagnostics(String text) {
    for (String line : text.split("\n")) {
      java.time.Instant.parse(line.split("\"t\":\"")[1].split("\"")[0]);
      check(line.matches("\\{\"schemaVersion\":1,\"t\":\"[^\"]+Z\",\"client\":\"c1\",\"applicationConnectionAttempt\":1,\"event\":\"application_(connection_(attempted|returned|failed|close_attempted|closed|close_failed)|reconnect_policy_stopped)\",\"stage\":\"(driver_open|configure|statement|read|rollback|commit)\"(,\"sqlState\":\"(08006|unknown)\",\"vendorCode\":73)?}"), "diagnostics contain only approved fields: " + line);
    }
    check(!text.contains(SECRET), "diagnostics never expose credentials, exception text or connection identity");
  }

  private static Result launch(String scenario, String policy, String diagnostic, String command) throws Exception {
    Path directory = Files.createTempDirectory("workload-policy-");
    Path counts = directory.resolve("counts");
    Path path = directory.resolve("application.jsonl");
    if ("diagnostic_open".equals(diagnostic)) {
      path = directory;
    } else if ("diagnostic_flush".equals(diagnostic)) {
      path = Path.of("/dev/full");
    } else if (List.of("diagnostic_returned", "diagnostic_ack", "diagnostic_failed", "diagnostic_unknown").contains(scenario)) {
      Process fifo = new ProcessBuilder("mkfifo", path.toString()).start();
      equal(0, fifo.waitFor(), "create FIFO diagnostic failure fixture");
    }
    ProcessBuilder builder = new ProcessBuilder(
        Path.of(System.getProperty("java.home"), "bin", "java").toString(),
        "-ea", "-cp", System.getProperty("java.class.path"),
        WorkloadApplicationReconnectTest.class.getName(), "child", scenario, command);
    Map<String, String> environment = builder.environment();
    for (String key : List.of(
        "JDBC_URL", "DB_USER", "DB_PASSWORD", "CLIENT_ID", "ENDPOINT", "OPS", "START_SEQ",
        "INTERVAL_MS", "ROLLBACK_EVERY", "READ_EVERY", "APP_RECONNECT_POLICY",
        "APP_CONNECTION_DIAGNOSTICS")) {
      environment.remove(key);
    }
    environment.put("COUNTS_FILE", counts.toString());
    environment.put("OPS", "3");
    environment.put("ROLLBACK_EVERY", scenario.equals("rollback") ? "1" : "0");
    environment.put("READ_EVERY", scenario.startsWith("read_") ? "2" : "0");
    if (!scenario.equals("capabilities")) {
      environment.put("JDBC_URL", URL);
      environment.put("DB_PASSWORD", SECRET);
      environment.put("DB_USER", SECRET);
    }
    if (policy != null) environment.put("APP_RECONNECT_POLICY", policy);
    if (diagnostic != null) environment.put("APP_CONNECTION_DIAGNOSTICS", path.toString());
    builder.redirectOutput(directory.resolve("stdout").toFile());
    builder.redirectError(directory.resolve("stderr").toFile());
    Process process = builder.start();
    if (!process.waitFor(15, TimeUnit.SECONDS)) {
      process.destroyForcibly();
      throw new AssertionError("child timed out: " + scenario);
    }
    String diag = Files.isRegularFile(path) ? Files.readString(path) : "";
    Result result = new Result(
        process.exitValue(), Files.readString(directory.resolve("stdout")),
        Files.readString(directory.resolve("stderr")), diag,
        Files.exists(counts) ? Files.readString(counts) : "");
    try (var files = Files.walk(directory)) {
      for (Path file : files.sorted(java.util.Comparator.reverseOrder()).toList()) Files.delete(file);
    }
    return result;
  }

  private record Result(int exit, String out, String err, String diag, String counts) {
    int count(String key) {
      for (String line : counts.split("\n")) {
        if (line.startsWith(key + "=")) return Integer.parseInt(line.substring(key.length() + 1));
      }
      return 0;
    }
  }

  private static void child(String scenario, String command) throws Exception {
    FakeDriver driver = new FakeDriver(scenario);
    DriverManager.registerDriver(driver);
    Runtime.getRuntime().addShutdownHook(new Thread(() -> {
      try {
        Files.writeString(Path.of(System.getenv("COUNTS_FILE")), "opens=" + driver.opens + "\ncloses=" + driver.closes + "\ntransportAttempts=" + driver.transportAttempts + "\n");
      } catch (IOException e) {
        throw new AssertionError(e);
      }
    }));
    if (scenario.startsWith("diagnostic_") && !List.of("diagnostic_open", "diagnostic_flush").contains(scenario)) {
      Thread reader = new Thread(() -> {
        try (BufferedReader input = Files.newBufferedReader(Path.of(System.getenv("APP_CONNECTION_DIAGNOSTICS")))) {
          input.readLine();
          if (!scenario.equals("diagnostic_returned")) input.readLine();
        } catch (IOException e) {
          throw new AssertionError(e);
        } finally {
          driver.readerClosed.countDown();
        }
      });
      reader.setDaemon(true);
      reader.start();
    }
    Workload.main(command.equals("marker") ? new String[] {command, "name", "value"} : new String[] {command});
  }

  private static final class FakeDriver implements Driver, InvocationHandler {
    private final String scenario;
    private final CountDownLatch readerClosed = new CountDownLatch(1);
    private int opens;
    private int closes;
    private int transportAttempts;
    private boolean injected;
    private boolean rowRead;
    private boolean select;
    private boolean transportReplaced;

    FakeDriver(String scenario) {
      this.scenario = scenario;
    }

    private void fail(String stage) throws SQLException {
      boolean match = scenario.equals(stage)
          || (stage.equals("statement") && List.of("close_failed", "bad_sqlstate", "diagnostic_failed").contains(scenario))
          || (stage.equals("commit") && List.of("close_unknown", "diagnostic_unknown").contains(scenario))
          || (stage.equals("read") && scenario.equals("read_query"));
      if (match && !injected) {
        injected = true;
        throw new SQLException(SECRET, scenario.equals("bad_sqlstate") ? SECRET : "08006", 73);
      }
    }

    private void awaitReader() throws SQLException {
      if (scenario.startsWith("diagnostic_")) {
        try {
          if (!readerClosed.await(5, TimeUnit.SECONDS)) throw new SQLException("FIFO fixture timeout");
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
          throw new SQLException(e);
        }
      }
    }

    private <T> T proxy(Class<T> type) {
      return type.cast(Proxy.newProxyInstance(getClass().getClassLoader(), new Class<?>[] {type}, this));
    }

    @Override
    public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
      String name = method.getName();
      if (name.equals("toString")) return SECRET;
      if (name.equals("close")) {
        if (proxy instanceof Connection) {
          closes++;
          if (scenario.startsWith("close_")) throw new SQLException(SECRET, "08006", 73);
        }
        if (proxy instanceof PreparedStatement) fail("statement_close");
        return null;
      }
      if (name.equals("setAutoCommit")) {
        fail("configure");
        return null;
      }
      if (name.equals("prepareStatement")) {
        fail("prepare");
        select = ((String) args[0]).startsWith("SELECT");
        return proxy(PreparedStatement.class);
      }
      if (name.equals("setString") || name.equals("setInt")) {
        fail("bind");
        return null;
      }
      if (name.equals("executeUpdate")) {
        awaitReader();
        if (scenario.equals("abrupt")) {
          Files.writeString(Path.of(System.getenv("APP_CONNECTION_DIAGNOSTICS")), "{\"schemaVersion\":", java.nio.file.StandardOpenOption.APPEND);
          Runtime.getRuntime().halt(7);
        }
        fail("statement");
        if (scenario.equals("internal_retry") && !transportReplaced) {
          // The first transport fails inside the driver; replacement and retry stay invisible.
          transportAttempts++;
          transportReplaced = true;
        }
        return 1;
      }
      if (name.equals("rollback")) {
        fail("rollback");
        return null;
      }
      if (name.equals("commit")) {
        if (select && List.of("read_commit", "read_mismatch_commit").contains(scenario)) throw new SQLException(SECRET, "08006", 73);
        fail("commit");
        return null;
      }
      if (name.equals("executeQuery")) {
        fail("read");
        rowRead = false;
        return proxy(ResultSet.class);
      }
      if (name.equals("next")) {
        fail("read_result");
        boolean result = !rowRead && !scenario.startsWith("read_mismatch");
        rowRead = true;
        return result;
      }
      if (name.equals("getString")) return ((Integer) args[0]) == 1 ? "c1" : "c1-1";
      if (name.equals("getInt")) return ((Integer) args[0]) == 2 ? 1 : 38;
      throw new AssertionError("Unexpected fake JDBC call: " + name);
    }

    @Override
    public Connection connect(String url, Properties info) throws SQLException {
      if (!acceptsURL(url)) return null;
      opens++;
      if (scenario.startsWith("capabilities")) throw new AssertionError("Capabilities connected");
      fail("driver_open");
      if (scenario.equals("diagnostic_returned")) awaitReader();
      transportAttempts++;
      return proxy(Connection.class);
    }

    @Override
    public boolean acceptsURL(String url) {
      return url != null && url.startsWith("jdbc:workload-policy-test:");
    }

    @Override
    public DriverPropertyInfo[] getPropertyInfo(String url, Properties info) {
      return new DriverPropertyInfo[0];
    }

    @Override
    public int getMajorVersion() {
      return 1;
    }

    @Override
    public int getMinorVersion() {
      return 0;
    }

    @Override
    public boolean jdbcCompliant() {
      return false;
    }

    @Override
    public Logger getParentLogger() {
      return Logger.getGlobal();
    }

  }

  private static String normalize(String text) {
    return text.replaceAll("\"t\":\"[^\"]+\"", "\"t\":\"TIME\"");
  }

  private static String history(String outcome, String error, int seq) {
    String prefix = "{\"t\":\"TIME\",\"client\":\"c1\",\"seq\":" + seq
        + ",\"op\":\"insert\",\"opId\":\"c1-00000" + seq
        + "\",\"endpoint\":\"rw\",\"event\":\"";
    return prefix + "attempted\",\"amount\":" + (seq * 37 + 1)
        + ",\"note\":\"c1-" + seq + "\"}\n" + prefix + outcome + "\""
        + (error == null ? "" : ",\"error\":\"" + error + "\"") + "}\n";
  }

  private static int occurrences(String text, String needle) {
    return (text.length() - text.replace(needle, "").length()) / needle.length();
  }

  private static void check(boolean condition, String message) {
    checks++;
    if (!condition) throw new AssertionError(message);
  }
  private static void equal(Object expected, Object actual, String message) {
    check(expected.equals(actual), message + ": expected " + expected + ", got " + actual);
  }
}
