htmlencode = require 'htmlencode'
$ = require 'jquery'
globals = require 'globals'
nick = require '../util/nick'
kd = require 'kd'
KDCustomHTMLView = kd.CustomHTMLView
KDHitEnterInputView = kd.HitEnterInputView
KDListViewController = kd.ListViewController
KDLoaderView = kd.LoaderView
KDModalView = kd.ModalView
KDView = kd.View
DomainItem = require './domainitem'


module.exports = class ManageDomainsView extends KDView

  constructor: (options = {}, data) ->

    options.cssClass = kd.utils.curry 'domains-view', options.cssClass

    super options, data

    {@machine} = @getOptions()

    @domainSuffix = ".#{nick()}.#{globals.config.userSitesDomain}"

    @addSubView @inputView = new KDView
      cssClass          : 'input-view'

    @inputView.addSubView @input = new KDHitEnterInputView
      type              : 'text'
      attributes        :
        spellcheck      : no
      callback          : @bound 'addDomain'

    @inputView.addSubView new KDView
      partial           : @domainSuffix
      cssClass          : 'domain-suffix'

    topDomain = "#{nick()}.#{globals.config.userSitesDomain}"

    @domainController   = new KDListViewController
      viewOptions       :
        type            : 'domain'
        wrapper         : yes
        itemClass       : DomainItem
        itemOptions     :
          machineId     : @machine._id

    @addSubView @domainController.getView()
    @inputView.hide()

    @domainController.getListView()
      .on 'DeleteDomainRequested', @bound 'removeDomain'
      .on 'DomainStateChanged',    @bound 'changeDomainStateRequested'

    @addSubView @loader = new KDLoaderView
      cssClass      : 'in-progress'
      size          :
        width       : 14
        height      : 14
      loaderOptions :
        color       : '#333333'
      showLoader    : yes

    @addSubView @warning = new KDCustomHTMLView
      cssClass      : 'warning hidden'
      click         : -> @hide()

    @listDomains()


  listDomains: (reset = no)->

    {computeController} = kd.singletons
    computeController.domains = []  if reset
    computeController.fetchDomains (err, domains = [])=>
      kd.warn err  if err?
      @domainController.replaceAllItems domains
      @loader.hide()


  addDomain:->

    {computeController} = kd.singletons

    domain = "#{htmlencode.XSSEncode @input.getValue().trim()}#{@domainSuffix}"
    machineId = @machine._id

    if @domainController.getItemCount() >= 5
      @warning.setTooltip
        title: "It's not allowed to create more than 5 domains."
      @warning.show()
      return

    @loader.show()
    @warning.hide()
    @input.makeDisabled()

    computeController.getKloud()

      .addDomain {domainName: domain, machineId}

      .then =>
        @domainController.addItem { domain, machineId }
        computeController.domains = []

        @input.setValue ""
        @inputView.hide()
        @emit "DomainInputCancelled"

      .catch (err)=>
        kd.warn "Failed to create domain:", err
        @warning.setTooltip title: err.message
        @warning.show()
        @input.setFocus()

      .finally =>
        @input.makeEnabled()
        @loader.hide()


  removeDomain:(domainItem)->

    {computeController} = kd.singletons

    {domain}  = domainItem.getData()
    machineId = @machine._id

    @loader.show()
    @warning.hide()

    computeController.getKloud()

      .removeDomain {domainName: domain, machineId}

      .then =>
        @domainController.removeItem domainItem
        computeController.domains = []

      .catch (err)=>
        kd.warn "Failed to remove domain:", err
        @warning.setTooltip title: err.message
        @warning.show()

      .finally =>
        @loader.hide()


  changeDomainStateRequested:(domainItem, state)->

    {stateToggle} = domainItem
    stateToggle.makeDisabled()

    @loader.show()
    @warning.hide()

    @askForPermission domainItem, state, (approved)=>
      if approved
        @changeDomainState domainItem, state
      else
        @revertToggle domainItem, state
        @loader.hide()
        stateToggle.makeEnabled()


  changeDomainState: (domainItem, state)->

    {computeController} = kd.singletons
    {stateToggle}       = domainItem
    {domain}            = domainItem.getData()
    machineId           = @machine._id
    action              = if state then 'setDomain' else 'unsetDomain'

    computeController.getKloud()[action] {domainName: domain, machineId}
      .then ->
        computeController.domains = []
        domainItem.data.machineId = null  unless state

      .catch (err)=>
        kd.warn "Failed to change domain state:", err
        @warning.setTooltip title: err.message
        @warning.show()

        @revertToggle domainItem, state

      .finally =>
        @loader.hide()
        stateToggle.makeEnabled()


  revertToggle:(domainItem, state)->

    {stateToggle} = domainItem
    if state
    then stateToggle.setOff no
    else stateToggle.setOn  no


  askForPermission:(domainItem, state, callback)->

    {domain, machineId} = domainItem.getData()

    if not state or not machineId? then return callback yes

    {computeController} = kd.singletons

    machineName = ""
    for machine in computeController.machines
      if machine._id is machineId
        machineName = " (<b>#{machine.getName()}</b>)"
        break

    modal = new KDModalView
      cssClass      : 'domain-assign-modal'
      title         : "Reassign domain ?"
      content       :
        """
        <div class='modalformline'>
          <p>
            The domain: <b>#{domain}</b> is already assigned to one
            of your other VM#{machineName}. If you continue, it will be
            reassigned to this VM and disabled on the other one.
          </p>
          <p>Continue?</p>
        </div>
        """
      overlay       : yes
      cancel        : ->
        modal.destroy()
        callback no
      buttons       :
        OK          :
          title     : "Yes"
          style     : 'solid red medium'
          loader    :
            color   : 'darkred'
          callback  : ->
            modal.destroy()
            callback yes
        cancel      :
          title     : "Cancel"
          style     : 'solid light-gray medium'
          callback  : -> modal.cancel()


  toggleInput:->

    @inputView.toggleClass 'hidden'

    {windowController} = kd.singletons

    windowController.addLayer @input
    @input.setFocus()

    @input.off  "ReceivedClickElsewhere"
    @input.once "ReceivedClickElsewhere", (event)=>
      return  if $(event.target).hasClass 'domain-toggle'
      @emit "DomainInputCancelled"
      @inputView.hide()
      @warning.hide()


