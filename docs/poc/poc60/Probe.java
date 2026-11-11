import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.List;

/**
 * POC-60 client: one JDBC connection through the read-write Broker, used as the workload
 * client of test/workload/client uses its connection: autocommit off, one transaction per
 * operation, and a connection that failed is closed and a new one opened for the next
 * operation. It reads one command per line on stdin and prints one JSON line per command:
 *
 * <pre>
 *   ins KEY          INSERT INTO t (k, at_ms) VALUES (KEY, now), then COMMIT
 *   read K1,K2,...   SELECT k FROM t WHERE k IN (...), then COMMIT
 *   quit
 * </pre>
 *
 * <p>Results: "ack" (the operation and its COMMIT returned), "failed" (an error before the
 * COMMIT was sent: nothing was committed), "unknown" (the COMMIT failed: the outcome is not
 * known). Times are epoch milliseconds.
 */
public final class Probe {
  private final String url;
  private Connection connection;
  private int opened;

  private Probe(String url) {
    this.url = url;
  }

  public static void main(String[] args) throws Exception {
    Probe p = new Probe(args[0]);
    BufferedReader in = new BufferedReader(new InputStreamReader(System.in, "UTF-8"));
    System.out.println("{\"cmd\":\"ready\",\"t\":" + System.currentTimeMillis() + "}");
    System.out.flush();
    String line;
    while ((line = in.readLine()) != null) {
      line = line.trim();
      if (line.isEmpty()) {
        continue;
      }
      if (line.equals("quit")) {
        break;
      }
      String[] parts = line.split(" ", 2);
      String out;
      if (parts[0].equals("ins")) {
        out = p.insert(parts[1]);
      } else if (parts[0].equals("read")) {
        out = p.read(parts[1]);
      } else {
        out = "{\"cmd\":\"bad\",\"line\":" + q(line) + "}";
      }
      System.out.println(out);
      System.out.flush();
    }
    p.discard();
  }

  private Connection connect() throws SQLException {
    if (connection == null) {
      Connection c = DriverManager.getConnection(url, "dba", "");
      c.setAutoCommit(false);
      connection = c;
      opened++;
    }
    return connection;
  }

  private void discard() {
    if (connection != null) {
      try {
        connection.close();
      } catch (SQLException ignored) {
        // The connection is thrown away because it failed.
      }
      connection = null;
    }
  }

  private String insert(String key) {
    long t0 = System.currentTimeMillis();
    int conn = opened;
    try {
      Connection c = connect();
      conn = opened;
      try (PreparedStatement s = c.prepareStatement("INSERT INTO t (k, at_ms) VALUES (?, ?)")) {
        s.setString(1, key);
        s.setLong(2, t0);
        s.executeUpdate();
      }
    } catch (SQLException e) {
      discard();
      return result("ins", key, t0, conn, "failed", null, e);
    }
    try {
      connection.commit();
    } catch (SQLException e) {
      discard();
      return result("ins", key, t0, conn, "unknown", null, e);
    }
    return result("ins", key, t0, conn, "ack", null, null);
  }

  private String read(String keys) {
    long t0 = System.currentTimeMillis();
    int conn = opened;
    String[] ks = keys.split(",");
    StringBuilder marks = new StringBuilder();
    for (int i = 0; i < ks.length; i++) {
      marks.append(i == 0 ? "?" : ",?");
    }
    List<String> found = new ArrayList<>();
    StringBuilder hdr = new StringBuilder();
    try {
      Connection c = connect();
      conn = opened;
      try (PreparedStatement s =
          c.prepareStatement("SELECT k FROM t WHERE k IN (" + marks + ") ORDER BY k")) {
        for (int i = 0; i < ks.length; i++) {
          s.setString(i + 1, ks[i]);
        }
        try (ResultSet r = s.executeQuery()) {
          while (r.next()) {
            found.add(r.getString(1));
          }
        }
      }
      // Which server answered: the log header of the server the CAS is connected to
      // holds its HA state and its log end, which differ between members.
      try (PreparedStatement s = c.prepareStatement("SHOW LOG HEADER");
          ResultSet r = s.executeQuery()) {
        if (r.next()) {
          for (int i = 1; i <= r.getMetaData().getColumnCount(); i++) {
            String name = r.getMetaData().getColumnLabel(i);
            if (name.equalsIgnoreCase("Ha_server_state") || name.equalsIgnoreCase("Append_lsa")
                || name.equalsIgnoreCase("Db_creation") || name.equalsIgnoreCase("Ha_file_status")) {
              hdr.append(hdr.length() == 0 ? "" : ",").append(q(name)).append(":")
                  .append(q(r.getString(i)));
            }
          }
        }
      } catch (SQLException e) {
        hdr.append(hdr.length() == 0 ? "" : ",").append("\"error\":").append(e.getErrorCode());
      }
      c.commit();
    } catch (SQLException e) {
      discard();
      return result("read", keys, t0, conn, "failed", found, e, hdr.toString());
    }
    return result("read", keys, t0, conn, "ack", found, null, hdr.toString());
  }

  private static String result(
      String cmd, String arg, long t0, int conn, String res, List<String> found, SQLException e) {
    return result(cmd, arg, t0, conn, res, found, e, "");
  }

  private static String result(
      String cmd,
      String arg,
      long t0,
      int conn,
      String res,
      List<String> found,
      SQLException e,
      String hdr) {
    StringBuilder b = new StringBuilder();
    b.append("{\"cmd\":").append(q(cmd)).append(",\"arg\":").append(q(arg));
    b.append(",\"t0\":").append(t0).append(",\"t1\":").append(System.currentTimeMillis());
    b.append(",\"conn\":").append(conn).append(",\"res\":").append(q(res));
    if (found != null) {
      b.append(",\"found\":[");
      for (int i = 0; i < found.size(); i++) {
        b.append(i == 0 ? "" : ",").append(q(found.get(i)));
      }
      b.append("]");
    }
    if (!hdr.isEmpty()) {
      b.append(",\"hdr\":{").append(hdr).append("}");
    }
    if (e != null) {
      b.append(",\"code\":").append(e.getErrorCode()).append(",\"msg\":").append(q(e.getMessage()));
    }
    return b.append("}").toString();
  }

  private static String q(String s) {
    if (s == null) {
      return "null";
    }
    StringBuilder b = new StringBuilder("\"");
    for (char ch : s.toCharArray()) {
      if (ch == '"' || ch == '\\') {
        b.append('\\').append(ch);
      } else if (ch < 0x20) {
        b.append(String.format("\\u%04x", (int) ch));
      } else {
        b.append(ch);
      }
    }
    return b.append('"').toString();
  }
}
