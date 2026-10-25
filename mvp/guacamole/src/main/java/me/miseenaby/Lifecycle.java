package me.miseenaby;
import java.sql.*;
import java.util.concurrent.*;
import org.apache.guacamole.GuacamoleException;
import org.apache.guacamole.GuacamoleSecurityException;
import org.apache.guacamole.net.GuacamoleTunnel;
import org.apache.guacamole.net.event.*;
import org.apache.guacamole.net.event.listener.Listener;

/** Supported Guacamole extension events enforce MEA lease/session revocation.
 * The bridge supplies identity, JDBC remains the connection permission registry.
 */
public class Lifecycle implements Listener {
 private final ConcurrentMap<String,GuacamoleTunnel> tunnels=new ConcurrentHashMap<>();
 private final ScheduledExecutorService scheduler=Executors.newSingleThreadScheduledExecutor(r->{Thread t=new Thread(r,"mea-lease-watch");t.setDaemon(true);return t;});
 public Lifecycle(){scheduler.scheduleWithFixedDelay(this::check,5,5,TimeUnit.SECONDS);}
 private Connection db() throws SQLException {
  java.util.Properties p=new java.util.Properties();p.setProperty("user",System.getenv("MEA_DB_USER"));p.setProperty("password",System.getenv("MEA_DB_PASSWORD"));p.setProperty("connectTimeout","3");p.setProperty("socketTimeout","3");
  return new org.postgresql.Driver().connect(System.getenv("MEA_JDBC_URL"),p);
 }
 @Override public void handleEvent(Object event)throws GuacamoleException {
  if(event instanceof TunnelConnectEvent){
   TunnelConnectEvent e=(TunnelConnectEvent)event;
   if(e.getAuthenticatedUser()==null)throw new GuacamoleSecurityException("MEA sign-in required");
   String username=e.getAuthenticatedUser().getIdentifier(),id=e.getTunnel().getUUID().toString();
   try(Connection c=db();PreparedStatement p=c.prepareStatement(
    "UPDATE mea.desktop_sessions SET tunnel_id=?,connected_at=now() WHERE id=("+
    "SELECT ds.id FROM mea.desktop_sessions ds JOIN mea.desktops d ON d.id=ds.desktop_id JOIN mea.users u ON u.id=d.user_id JOIN mea.sessions s ON s.hash=ds.web_session_hash "+
    "WHERE u.username=? AND NOT u.disabled AND d.state='ready' AND d.expires_at>now() AND ds.closed_at IS NULL AND ds.tunnel_id IS NULL AND ds.created_at>now()-interval '35 seconds' "+
    "AND s.revoked_at IS NULL AND s.expires_at>now() ORDER BY ds.created_at DESC LIMIT 1 FOR UPDATE OF ds SKIP LOCKED)")){
    p.setString(1,id);p.setString(2,username);if(p.executeUpdate()!=1)throw new GuacamoleSecurityException("MEA desktop session is not authorized");
    tunnels.put(id,e.getTunnel());
   }catch(SQLException ex){System.err.println("MEA lifecycle SQL failure "+ex.getSQLState()+": "+ex.getMessage());throw new GuacamoleSecurityException("MEA authorization unavailable");}
  } else if(event instanceof TunnelCloseEvent){
   String id=((TunnelCloseEvent)event).getTunnel().getUUID().toString();tunnels.remove(id);closeRecord(id);
  }
 }
 private void closeRecord(String id){
  try(Connection c=db();PreparedStatement p=c.prepareStatement("UPDATE mea.desktop_sessions SET closed_at=COALESCE(closed_at,now()) WHERE tunnel_id=?")){p.setString(1,id);p.executeUpdate();}catch(SQLException ignored){}
 }
 private void check(){
  if(tunnels.isEmpty())return;
  for(java.util.Map.Entry<String,GuacamoleTunnel> entry:tunnels.entrySet()){
   boolean allowed=false;
   try(Connection c=db();PreparedStatement p=c.prepareStatement(
    "SELECT 1 FROM mea.desktop_sessions ds JOIN mea.desktops d ON d.id=ds.desktop_id JOIN mea.sessions s ON s.hash=ds.web_session_hash JOIN mea.users u ON u.id=d.user_id "+
    "WHERE ds.tunnel_id=? AND ds.closed_at IS NULL AND d.state='ready' AND d.expires_at>now() AND d.last_activity>now()-interval '20 minutes' "+
    "AND (d.last_heartbeat IS NULL OR d.last_heartbeat>now()-interval '90 seconds') AND NOT u.disabled AND s.revoked_at IS NULL AND s.expires_at>now()")){
    p.setString(1,entry.getKey());try(ResultSet rs=p.executeQuery()){allowed=rs.next();}
   }catch(SQLException ignored){}
   if(!allowed){try{entry.getValue().close();}catch(GuacamoleException ignored){}tunnels.remove(entry.getKey());closeRecord(entry.getKey());}
  }
 }
}

