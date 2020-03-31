package ldapsync

import (
	"fmt"
	"strings"

	"github.com/go-ldap/ldap"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
)

var Log *logrus.Logger

func init() {
	Log = logrus.New()
}

type SearchConfig struct {
	config    *map[string][]string
	filter    string
	attribute string
}

type LDAPSync struct {
	lc           *LDAPCaller
	uc           *UyuniCaller
	cr           *ConfigReader
	ldapusers    []*UyuniUser
	uyuniusers   []*UyuniUser
	allldapusers []*UyuniUser
	roleConfigs  [2]*SearchConfig
}

func NewLDAPSync(cfgpath string) *LDAPSync {
	sync := new(LDAPSync)
	sync.cr = NewConfigReader(cfgpath)
	sync.lc = NewLDAPCaller().
		SetHost(sync.cr.Config().Directory.Host).
		SetPort(sync.cr.Config().Directory.Port).
		SetUser(sync.cr.Config().Directory.User).
		SetPassword(sync.cr.Config().Directory.Password)

	sync.uc = NewUyuniCaller(sync.cr.Config().Spacewalk.Url, !sync.cr.Config().Spacewalk.Checkssl).
		SetUser(sync.cr.Config().Spacewalk.User).
		SetPassword(sync.cr.Config().Spacewalk.Password)
	sync.ldapusers = make([]*UyuniUser, 0)
	sync.uyuniusers = make([]*UyuniUser, 0)
	sync.allldapusers = make([]*UyuniUser, 0)

	sync.roleConfigs = [2]*SearchConfig{
		&SearchConfig{config: &sync.cr.Config().Directory.Roles,
			filter: "(objectClass=organizationalRole)", attribute: "roleOccupant"},
		&SearchConfig{config: &sync.cr.Config().Directory.Groups,
			filter: "(|(objectClass=groupOfNames)(objectClass=group))", attribute: "member"},
	}
	return sync
}

func (sync *LDAPSync) ConfigReader() *ConfigReader {
	return sync.cr
}

func (sync *LDAPSync) Start() *LDAPSync {
	sync.lc.Connect()

	sync.verifyIgnoredUsers()
	sync.refreshExistingUyuniUsers()
	sync.refreshStagedLDAPUsers()
	sync.refreshAllLDAPUsers()
	sync.refreshUyuniUsersStatus()

	return sync
}

func (sync *LDAPSync) Finish() {
	sync.lc.Disconnect()
}

// Helper function that looks for the same user or at least its ID
func (sync LDAPSync) in(user UyuniUser, users []*UyuniUser) bool {
	for _, u := range users {
		if u.Uid == user.Uid {
			return true
		}
	}
	return false
}

// Match a given user by a DN, compare all metadata.
func (sync LDAPSync) sameAsIn(user *UyuniUser, users []*UyuniUser) (bool, error) {
	for _, u := range users {
		if u.Uid == user.Uid {
			same := u.Email == user.Email
			if same {
				same = u.Name == user.Name
			} else {
				user.accountchanged = true
				Log.Debugf("User %s email has been changed from %s to %s", user.Uid, user.Email, u.Email)
			}

			if same {
				same = u.Secondname == user.Secondname
			} else {
				user.accountchanged = true
				Log.Debugf("User %s name has been changed from %s to %s", user.Uid, user.Name, u.Name)
			}

			if same {
				same = CompareRoles(user, u)
			} else {
				user.accountchanged = true
				Log.Debugf("User %s family name has been changed from %s to %s", user.Uid, user.Secondname, u.Secondname)
			}

			if !same {
				user.roleschanged = true
				Log.Debugf("User %s role set has been changed", user.Uid)
			}

			return same, nil
		}
	}

	Log.Debugf("Unable to compare user '%s': user not found", user.Uid)

	return false, fmt.Errorf("User UID %s was not found", user.Uid)
}

// Returns a copy of LDAP user by Uyuni user
func (sync *LDAPSync) updateFromLDAPUser(uyuniUser *UyuniUser) {
	for _, ldapUser := range sync.ldapusers {
		if ldapUser.Uid == uyuniUser.Uid {
			uyuniUser.Name, uyuniUser.Secondname, uyuniUser.Email = ldapUser.Name, ldapUser.Secondname, ldapUser.Email
			uyuniUser.FlushRoles()
			for _, role := range ldapUser.GetRoles() {
				uyuniUser.AddRoles(role)
			}
		}
	}
}

// GetDeletedUsers returns an array of users that has been deleted from Uyuni.
// They are both in Uyuni and LDAP, but they are not specified in the LDAP admin-related groups.
func (sync *LDAPSync) GetDeletedUsers() []*UyuniUser {
	var users []*UyuniUser
	for _, user := range sync.allldapusers {
		if !sync.in(*user, sync.ldapusers) && sync.in(*user, sync.uyuniusers) {
			users = append(users, user)
		}
	}

	return users
}

// GetNewUsers returns LDAP users that are not yet in the Uyuni
func (sync *LDAPSync) GetNewUsers() []*UyuniUser {
	var users []*UyuniUser
	for _, user := range sync.uyuniusers {
		if user.IsNew() {
			sync.updateFromLDAPUser(user)
			users = append(users, user)
		}
	}

	return users
}

// GetOutdatedUsers returns LDAP users that are in the Uyuni, but needs refresh
func (sync *LDAPSync) GetOutdatedUsers() []*UyuniUser {
	var users []*UyuniUser
	for _, user := range sync.uyuniusers {
		if !user.IsNew() && user.IsOutdated() {
			users = append(users, user)
		}
	}

	return users
}

// SyncUsers is creating new users in Uyuni by their names and emails.
func (sync *LDAPSync) SyncUsers() []*UyuniUser {
	Log.Info("Begin user synchronisation between LDAP and Uyuni server")

	failed := make([]*UyuniUser, 0)
	newUsers := sync.GetNewUsers()
	if len(newUsers) > 0 {
		Log.Debugf("Found %d new users", len(newUsers))
		for _, user := range newUsers {
			Log.Debugf("New user: %s", user.Uid)
			_, user.Err = sync.uc.Call("user.create", sync.uc.Session(), user.Uid, "", user.Name, user.Secondname, user.Email, 1)

			if !user.IsValid() {
				failed = append(failed, user)
				Log.Debugf("Failed to create user %s due to %s", user.Uid, user.Err.Error())
			} else {
				sync.pushUserRolesToUyuni(user)
			}
		}
	}

	existingUsers := sync.GetOutdatedUsers()
	if len(existingUsers) > 0 {
		Log.Debugf("Updating %d users", len(existingUsers))
		for _, user := range existingUsers {
			Log.Debugf("Update data for user: %s", user.Uid)
			sync.pushUserRolesToUyuni(user)
			sync.pushUserAccountDataToUyuni(user)
		}
	}

	deletedUsers := sync.GetDeletedUsers()
	if len(deletedUsers) > 0 {
		Log.Debugf("Deleting removed %d users", len(deletedUsers))
		for _, user := range deletedUsers {
			Log.Debugf("Remove user: %s", user.Uid)
			sync.deleteUser(user)
		}
	}

	Log.Infof("Added %d new users, updated %d existing users, removed %d users", len(newUsers), len(existingUsers), len(deletedUsers))
	Log.Info("End user synchronisation between LDAP and Uyuni server")

	return failed
}

// Remove user from the Uyuni
func (sync *LDAPSync) deleteUser(uyuniUser *UyuniUser) {
	_, err := sync.uc.Call("user.delete", sync.uc.Session(), uyuniUser.Uid)
	if err != nil {
		Log.Errorf("Cannot delete users '%s': %s", uyuniUser.Uid, err.Error())
	}
}

// Push account data to Uyuni
func (sync *LDAPSync) pushUserAccountDataToUyuni(user *UyuniUser) {
	_, err := sync.uc.Call("user.setDetails", sync.uc.Session(), user.Uid, map[string]string{
		"first_name": user.Name, "last_name": user.Secondname, "email": user.Email})
	if err != nil {
		Log.Errorf("Failed to push user account data for %s: %s", user.Uid, err.Error())
		return
	}

	_, err = sync.uc.Call("user.usePamAuthentication", sync.uc.Session(), user.Uid, 1)
	if err != nil {
		Log.Errorf("Failed to push user authentication settings for %s: %s", user.Uid, err.Error())
		return
	}
}

// Sync user roles
func (sync *LDAPSync) pushUserRolesToUyuni(uyuniUser *UyuniUser) {
	// Remove current roles away
	ret, err := sync.uc.Call("user.listRoles", sync.uc.Session(), uyuniUser.Uid)
	if err != nil {
		Log.Errorf("Cannot list roles for user '%s': %s", uyuniUser.Uid, err.Error())
		return
	}

	for _, role := range ret.([]interface{}) {
		_, err := sync.uc.Call("user.removeRole", sync.uc.Session(), uyuniUser.Uid, role.(string))
		if err != nil {
			Log.Errorf("Cannot remove role '%s': %s", role, err.Error())
		} else {
			Log.Debugf("Removed role '%s'", role)
		}
	}

	// Add new roles
	for _, role := range uyuniUser.GetRoles() {
		_, err := sync.uc.Call("user.addRole", sync.uc.Session(), uyuniUser.Uid, role)
		if err != nil {
			Log.Errorf("Cannot add role '%s': %s", role, err.Error())
		} else {
			Log.Debugf("Added role '%s'", role)
		}
	}
}

// Iterate over possible attribute aliases
func (sync LDAPSync) getAttributes(entry *ldap.Entry, attr ...string) string {
	for _, a := range attr {
		obj := entry.GetAttributeValue(a)
		if obj != "" {
			return obj
		}
	}

	return ""
}

// At least one ignored/frozen user must have org_admin role
func (sync *LDAPSync) verifyIgnoredUsers() {
	valid := false
	for _, uid := range sync.cr.Config().Directory.Frozen {
		res, err := sync.uc.Call("user.listRoles", sync.uc.Session(), uid)
		if err != nil {
			Log.Errorf("No users has been found with the UID '%s'", uid)
		} else {
			for _, role := range res.([]interface{}) {
				if role.(string) == "org_admin" {
					valid = true
					goto End
				}
			}
		}
	}
End:
	if !valid {
		Log.Fatal("In Uyuni server no actual frozen accounts found with the role 'org_admin'. " +
			"You are risking permanently locking Uyuni server, if you have incorrect LDAP users settings.")
	}
}

// Refresh what users are new and what needs update
func (sync *LDAPSync) refreshUyuniUsersStatus() []*UyuniUser {
	var newusers []*UyuniUser
	for _, user := range sync.ldapusers {
		user.new = !sync.in(*user, sync.uyuniusers)
		if !user.IsNew() {
			isSame, err := sync.sameAsIn(user, sync.uyuniusers)
			if err == nil {
				user.outdated = !isSame
			}
		} else {
			newusers = append(newusers, user.Clone())
		}

		for _, uUuser := range sync.uyuniusers {
			if uUuser.Uid == user.Uid {
				uUuser.outdated = user.outdated
				uUuser.Name = user.Name
				uUuser.Secondname = user.Secondname
				uUuser.Email = user.Email
				uUuser.FlushRoles().AddRoles(user.GetRoles()...)
			}
		}
	}

	sync.uyuniusers = append(sync.uyuniusers, newusers...)

	return sync.uyuniusers
}

// Get all existing users in Uyuni.
func (sync *LDAPSync) refreshExistingUyuniUsers() []*UyuniUser {
	sync.uyuniusers = nil
	res, err := sync.uc.Call("user.listUsers", sync.uc.Session())
	if err != nil {
		Log.Fatal(err)
	}
	for _, usrdata := range res.([]interface{}) {
		uid := usrdata.(map[string]interface{})["login"].(string)
		if funk.Contains(sync.cr.Config().Directory.Frozen, uid) {
			continue
		}

		user := NewUyuniUser()
		user.Uid = uid

		res, err = sync.uc.Call("user.getDetails", sync.uc.Session(), user.Uid)
		if err != nil {
			Log.Fatal(err)
		}
		userDetails := res.(map[string]interface{})

		user.Email = userDetails["email"].(string)
		user.Name = userDetails["first_name"].(string)
		user.Secondname = userDetails["last_name"].(string)

		// Get user roles
		res, err = sync.uc.Call("user.listRoles", sync.uc.Session(), user.Uid)
		if err != nil {
			Log.Fatal(err)
		}

		for _, roleItf := range res.([]interface{}) {
			user.AddRoles(roleItf.(string))
		}

		sync.uyuniusers = append(sync.uyuniusers, user)
	}
	return sync.uyuniusers
}

// Get an attribute name for DN.
// This allows to substitute remapped fields from the configuration, returning
// new remapped name, or keep the original one.
func (sync *LDAPSync) getAttributeNameFor(attr string) string {
	if fieldmap, ext := sync.cr.Config().Directory.Attrmap[sync.cr.Config().Directory.Allusers]; ext {
		nAttr, ext := fieldmap[attr]
		if ext {
			attr = nAttr
		}
	}

	return attr
}

func (sync *LDAPSync) newUserFromDN(dn string) *UyuniUser {
	user := NewUyuniUser()
	request := ldap.NewSearchRequest(dn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{}, nil)

	entries := sync.lc.Search(request).Entries
	if len(entries) == 1 {
		entry := entries[0]
		user.Dn = entry.DN
		user.Uid = entry.GetAttributeValue(sync.getAttributeNameFor("uid"))
		user.Email = entry.GetAttributeValue(sync.getAttributeNameFor("mail"))

		cn := strings.Split(entry.GetAttributeValue("cn"), " ")
		if len(cn) == 2 {
			user.Name, user.Secondname = cn[0], cn[1]
		} else {
			user.Name = sync.getAttributes(entry, sync.getAttributeNameFor("name"), sync.getAttributeNameFor("givenName"))
			user.Secondname = entry.GetAttributeValue(sync.getAttributeNameFor("sn"))
		}
	} else {
		Log.Errorf("DN '%s' matches more or less than one distinct user", dn)
	}

	return user
}

// Get all users from LDAP, regardless are they are meant to be in the Uyuni
func (sync *LDAPSync) refreshAllLDAPUsers() []*UyuniUser {
	sync.allldapusers = nil
	request := ldap.NewSearchRequest(sync.cr.Config().Directory.Allusers,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=organizationalPerson)", []string{}, nil)

	for _, entry := range sync.lc.Search(request).Entries {
		sync.allldapusers = append(sync.allldapusers, sync.newUserFromDN(entry.DN))
	}

	return sync.allldapusers
}

// Get existing LDAP users, based on the groups mapping
func (sync *LDAPSync) refreshStagedLDAPUsers() []*UyuniUser {
	sync.ldapusers = nil
	udns := make(map[string]bool)

	// Get all *distinct* user DNs from the "member" attiribute across all the groups
	for _, roleset := range []map[string][]string{sync.cr.Config().Directory.Groups, sync.cr.Config().Directory.Roles} {
		for gdn := range roleset {
			request := ldap.NewSearchRequest(gdn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
				"(objectClass=*)", []string{}, nil)
			for _, entry := range sync.lc.Search(request).Entries {
				for _, udn := range append(entry.GetAttributeValues("member"), entry.GetAttributeValues("roleOccupant")...) {
					udns[udn] = true
				}
			}
		}
	}

	// Collect users data
	for udn := range udns {
		user := sync.newUserFromDN(udn)
		if user.Uid != "" && !funk.Contains(sync.cr.Config().Directory.Frozen, user.Uid) {
			sync.updateLDAPUserRoles(user)
			sync.ldapusers = append(sync.ldapusers, user)
		}
	}

	return sync.ldapusers
}

func (sync *LDAPSync) mergeRolesByAttributes(dn string, user *UyuniUser, filter string, attribute string, uyuniRoles []string) {
	req := ldap.NewSearchRequest(dn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, filter, []string{}, nil)
	for _, entry := range sync.lc.Search(req).Entries {
		for _, roleDn := range entry.GetAttributeValues(attribute) {
			if roleDn == user.Dn {
				user.AddRoles(uyuniRoles...)
			}
		}
	}
}

// Get LDAP organizationalRole based on configuration
func (sync *LDAPSync) updateLDAPUserRoles(user *UyuniUser) {
	for _, searchConfig := range sync.roleConfigs {
		for dn, uyuniRoles := range *searchConfig.config {
			sync.mergeRolesByAttributes(dn, user, searchConfig.filter, searchConfig.attribute, uyuniRoles)
		}
	}
}
